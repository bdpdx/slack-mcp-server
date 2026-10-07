package agentchat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Cohort liveness: agents working in a projects/ cohort register with their
// home's listener, which then watches for an unavailable GM (an @mention of
// the GM left unanswered, or a BLOCKED notice from it) and for 2-hourly
// checkpoints, and pushes a notice into the right session when one falls
// due. Sessions that never register get none of this.
//
// The listener reads the project from the rezilient-project-state checkout
// (the real directory behind a code clone's projects/ symlink):
// <root>/<project>/PROJECT.md for the succession order, gm.json for the
// current GM and term, and holds/<agent> for the user's holds. It never
// writes there; claims go through `chat gm claim`.

const (
	// gmAnswerDeadline is how long an @mention of the GM may go unanswered
	// before the GM counts as unavailable.
	gmAnswerDeadline = 15 * time.Minute
	// successorAckWindow is how long each successor has to act on the notice
	// before the next one is told.
	successorAckWindow = 10 * time.Minute
	// checkpointInterval is the drift and context checkpoint period.
	checkpointInterval = 2 * time.Hour
)

var (
	successionMarker = regexp.MustCompile(`<!--\s*cohort-succession:\s*([^>]*?)\s*-->`)
	successionProse  = regexp.MustCompile(`(?mi)^\W*GM succession order:\W*(.+)$`)
	agentName        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	projectName      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// GMState is projects/<project>/gm.json: the current GM authority.
type GMState struct {
	Term      int    `json:"term"`
	GM        string `json:"gm"`
	ClaimID   string `json:"claim_id,omitempty"`
	Since     string `json:"since,omitempty"`
	Reason    string `json:"reason,omitempty"`
	HandoffOf string `json:"handoff_of,omitempty"`
}

// CohortProject is what the listener knows about one project.
type CohortProject struct {
	Name       string
	Root       string
	Succession []string // GM succession order; also the roster
	GM         GMState
	holds      map[string]bool
}

// ParseSuccession reads the succession order from PROJECT.md: the
// `<!-- cohort-succession: a, b -->` marker, or else the template's
// "GM succession order:" line ("a (GM), then b, c.").
func ParseSuccession(projectMD string) ([]string, error) {
	var list string
	if m := successionMarker.FindStringSubmatch(projectMD); m != nil {
		list = m[1]
	} else if m := successionProse.FindStringSubmatch(projectMD); m != nil {
		list = m[1]
	} else {
		return nil, errors.New("PROJECT.md has no GM succession order")
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(strings.Trim(part, " .*`"))
		part = strings.TrimSpace(strings.TrimPrefix(part, "then "))
		name, _, _ := strings.Cut(part, " ")
		name = strings.Trim(name, "*`.")
		if name == "" {
			continue
		}
		if !agentName.MatchString(name) {
			return nil, fmt.Errorf("succession order: %q is not an agent name", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("succession order lists %s twice", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, errors.New("PROJECT.md's GM succession order is empty")
	}
	return out, nil
}

// LoadCohortProject reads project's PROJECT.md, gm.json and holds under root.
func LoadCohortProject(root, project string) (*CohortProject, error) {
	if !projectName.MatchString(project) {
		return nil, fmt.Errorf("%q is not a project name", project)
	}
	dir := filepath.Join(root, project)
	md, err := os.ReadFile(filepath.Join(dir, "PROJECT.md"))
	if err != nil {
		return nil, fmt.Errorf("project %s: %w", project, err)
	}
	order, err := ParseSuccession(string(md))
	if err != nil {
		return nil, fmt.Errorf("project %s: %w", project, err)
	}
	p := &CohortProject{Name: project, Root: root, Succession: order, GM: GMState{GM: order[0]}, holds: map[string]bool{}}
	data, err := os.ReadFile(filepath.Join(dir, "gm.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &p.GM); err != nil {
			return nil, fmt.Errorf("project %s: gm.json: %w", project, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("project %s: %w", project, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "holds"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("project %s: holds: %w", project, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			p.holds[e.Name()] = true
		}
	}
	return p, nil
}

// InRoster reports whether agent is in the project's succession order.
func (p *CohortProject) InRoster(agent string) bool {
	for _, a := range p.Succession {
		if a == agent {
			return true
		}
	}
	return false
}

// Held reports whether the user has put agent on hold.
func (p *CohortProject) Held(agent string) bool { return p.holds[agent] }

// Successors lists, in succession order, the agents that could take over
// from the current GM: everyone but the GM and the agents on hold.
func (p *CohortProject) Successors() []string {
	var out []string
	for _, a := range p.Succession {
		if a != p.GM.GM && !p.holds[a] {
			out = append(out, a)
		}
	}
	return out
}

// EscalationStep says which successor (0 for the first) should act, given
// when the GM was found unavailable-to-be (the mention or BLOCKED notice)
// and now; -1 means not yet. It depends only on time, so every listener
// reaches the same answer without talking to the others.
func EscalationStep(seen, now time.Time) int {
	elapsed := now.Sub(seen)
	if elapsed < gmAnswerDeadline {
		return -1
	}
	return int((elapsed - gmAnswerDeadline) / successorAckWindow)
}

// CohortReg is one agent session's registration in a project cohort.
type CohortReg struct {
	Project            string    `json:"project"`
	Agent              string    `json:"agent"`
	SessionID          string    `json:"session_id"`
	Root               string    `json:"root"`    // the project-state checkout
	Channel            string    `json:"channel"` // the project channel's ID
	RegisteredAt       time.Time `json:"registered_at"`
	LastCheckpoint     time.Time `json:"last_checkpoint,omitempty"`
	CheckpointNotified time.Time `json:"checkpoint_notified,omitempty"`
	OffDuty            bool      `json:"off_duty,omitempty"`
}

func cohortKey(session, project string) string { return session + "|" + project }

// checkpointAnchor is the start of the current checkpoint cycle: the last
// completed checkpoint, or registration if there has been none.
func (r *CohortReg) checkpointAnchor() time.Time {
	if r.LastCheckpoint.After(r.RegisteredAt) {
		return r.LastCheckpoint
	}
	return r.RegisteredAt
}

// CheckpointDue reports whether a checkpoint notice should go out now. One
// falls due every two hours after the anchor until the agent completes a
// checkpoint, and at most one goes out per due point: an agent that comes
// back after missing several gets one notice, not one per missed period.
func (r *CohortReg) CheckpointDue(now time.Time) bool {
	anchor := r.checkpointAnchor()
	periods := now.Sub(anchor) / checkpointInterval
	if periods < 1 {
		return false
	}
	latest := anchor.Add(periods * checkpointInterval)
	return r.CheckpointNotified.Before(latest)
}
