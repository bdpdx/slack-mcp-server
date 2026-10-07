package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/version"
	"github.com/slack-go/slack"
)

// Exit statuses of the cohort commands, so scripts (projects/bin/setup.sh)
// can tell failures apart.
const (
	exitCohortInvalid   = 2 // PROJECT.md missing or invalid, or agent not on the roster
	exitCohortNoSession = 3 // the session is not watching the project channel
)

// exitError carries a specific exit status out of a command.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

// capabilities are the cohort features this build supports, for
// `chat capabilities`; setup scripts check them instead of assuming.
var capabilities = []string{"blocked-v1", "cohort-v1", "gm-claim-v1"}

func (c *cli) capabilities() error {
	c.printJSON(map[string]any{"version": version.Version, "capabilities": capabilities})
	return nil
}

// projectStateRoot finds the rezilient-project-state checkout: the real
// directory behind the nearest projects/ symlink from dir upward.
func projectStateRoot(dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		link := filepath.Join(d, "projects")
		if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return filepath.EvalSymlinks(link)
		}
		if parent := filepath.Dir(d); parent == d {
			return "", errors.New("no projects/ symlink found from here upward; pass --project-root")
		}
	}
}

// projectStateOrigin is what a project-state checkout's origin must name:
// GM authority and holds are read from, and claims pushed to, that remote.
var projectStateOrigin = regexp.MustCompile(`^(git@github\.com:|ssh://git@github\.com/|https://github\.com/)rezilient-co/rezilient-project-state(\.git)?/?$`)

// checkProjectState refuses a root whose origin is not rezilient-project-state
// or that has no fetched origin/main.
func checkProjectState(ctx context.Context, dir string) error {
	url, err := gitIn(ctx, dir, nil, nil, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("%s has no origin remote", dir)
	}
	if !projectStateOrigin.MatchString(strings.TrimSpace(url)) {
		return fmt.Errorf("%s: origin %s is not rezilient-co/rezilient-project-state", dir, strings.TrimSpace(url))
	}
	if _, err := gitIn(ctx, dir, nil, nil, "rev-parse", "--verify", "-q", "refs/remotes/origin/main"); err != nil {
		return fmt.Errorf("%s is not a project-state checkout with origin/main", dir)
	}
	return nil
}

func (c *cli) cohort(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cohort register|leave|checkpoint|duty|status")
	}
	fs := flag.NewFlagSet("cohort", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	project := fs.String("project", "", "project name (its directory and Slack channel)")
	agent := fs.String("agent", "", "this agent's Slack name (register)")
	root := fs.String("project-root", "", "the rezilient-project-state checkout (default: from the projects/ symlink)")
	drift := fs.String("drift", "", "the drift line to post (checkpoint)")
	contextNote := fs.String("context", "", "a private note on the context self-check (checkpoint); kept in this home's local checkpoint log, never posted")
	format := fs.String("format", "json", "json or table (status)")
	rest, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	sub, sessErr := detectSession(os.Getenv)
	switch args[0] {
	case "status":
		req := ControlRequest{Op: "cohort-status"}
		if *project != "" {
			req.Cohort = &CohortReg{Project: *project}
		}
		resp, err := SendControl(ctx, c.home.ControlSocket, req)
		if err != nil {
			return errors.New("no slack-agent-chat listener is running")
		}
		if *format == "table" {
			tw := tabwriter.NewWriter(c.stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tAGENT\tSESSION\tDUTY\tNEXT CHECKPOINT")
			for _, r := range resp.Cohort {
				duty := "on"
				if r.OffDuty {
					duty = "off"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Project, r.Agent, r.SessionID, duty, r.nextCheckpoint().Local().Format(time.RFC3339))
			}
			return tw.Flush()
		}
		holds := map[string][]string{}
		authority := map[string]map[string]any{}
		for _, r := range resp.Cohort {
			if _, seen := authority[r.Project]; seen {
				continue
			}
			if p, err := LoadCohortProjectAt(ctx, r.Root, r.Project, "refs/remotes/origin/main"); err == nil {
				authority[r.Project] = map[string]any{"gm": p.GM.GM, "term": p.GM.Term, "claim_id": p.GM.ClaimID}
				hs := []string{}
				for _, a := range p.Succession {
					if p.Held(a) {
						hs = append(hs, a)
					}
				}
				holds[r.Project] = hs
			} else {
				authority[r.Project] = map[string]any{"error": err.Error()}
			}
		}
		c.printJSON(map[string]any{"ok": true, "cohort": cohortJSON(resp.Cohort), "authority": authority,
			"holds": holds, "pending": resp.Watches})
		return nil
	}
	if *project == "" {
		return fmt.Errorf("cohort %s needs --project", args[0])
	}
	if sessErr != nil {
		return exitError{exitCohortNoSession, sessErr}
	}
	req := ControlRequest{SessionID: sub.SessionID, Cohort: &CohortReg{Project: *project}}
	switch args[0] {
	case "register":
		if *agent == "" {
			return errors.New("cohort register needs --agent")
		}
		dir := *root
		if dir == "" {
			wd, _ := os.Getwd()
			if dir, err = projectStateRoot(wd); err != nil {
				return exitError{exitCohortInvalid, err}
			}
		}
		if dir, err = filepath.EvalSymlinks(dir); err != nil {
			return exitError{exitCohortInvalid, err}
		}
		// The root must be a project-state checkout with a fetched origin/main:
		// that is where GM authority and holds are read from.
		if err := checkProjectState(ctx, dir); err != nil {
			return exitError{exitCohortInvalid, err}
		}
		if _, err := LoadCohortProjectAt(ctx, dir, *project, "refs/remotes/origin/main"); err != nil {
			return exitError{exitCohortInvalid, err}
		}
		// An agent registers as itself: --agent must be this home's bot.
		me, err := c.identity(ctx)
		if err != nil {
			return exitError{exitCohortNoSession, err}
		}
		if me.agentName != *agent {
			return exitError{exitCohortInvalid, fmt.Errorf("--agent %s is not this home's agent (%s)", *agent, me.agentName)}
		}
		channel, _, err := c.resolveChannel(ctx, *project)
		if err != nil {
			return exitError{exitCohortNoSession, err}
		}
		req.Op, req.Cohort.Agent, req.Cohort.Root, req.Cohort.Channel = "cohort-register", *agent, dir, channel
	case "leave":
		req.Op = "cohort-leave"
	case "duty":
		if len(rest) != 1 || (rest[0] != "on" && rest[0] != "off") {
			return errors.New("usage: cohort duty on|off --project P")
		}
		req.Op, req.Text = "cohort-duty", rest[0]
	case "checkpoint":
		if strings.TrimSpace(*drift) == "" {
			return errors.New(`cohort checkpoint needs --drift "<line>"`)
		}
		// The public drift line is the checkpoint: post it first, and record
		// completion only once it is in the channel.
		reg, err := c.registration(ctx, sub.SessionID, *project)
		if err != nil {
			return err
		}
		if _, _, err := c.bot.PostMessageContext(ctx, reg.Channel,
			slack.MsgOptionText("Checkpoint: "+slackEscaper.Replace(strings.TrimSpace(*drift)), false)); err != nil {
			return fmt.Errorf("posting the drift line failed; the checkpoint is not recorded: %w", err)
		}
		req.Op = "cohort-checkpoint"
	default:
		return fmt.Errorf("unknown cohort command %q", args[0])
	}
	resp, err := SendControl(ctx, c.home.ControlSocket, req)
	if err != nil {
		return errors.New("no slack-agent-chat listener is running")
	}
	if !resp.OK {
		code := 1
		if req.Op == "cohort-register" {
			code = exitCohortInvalid
			if strings.Contains(resp.Error, "not watching") || strings.Contains(resp.Error, "does not watch") {
				code = exitCohortNoSession
			}
		}
		return exitError{code, errors.New(resp.Error)}
	}
	if req.Op == "cohort-checkpoint" && strings.TrimSpace(*contextNote) != "" {
		if err := c.logCheckpoint(*project, *drift, *contextNote); err != nil {
			fmt.Fprintf(c.stderr, "warning: the checkpoint is recorded, but its context note was not logged: %v\n", err)
		}
	}
	out := map[string]any{"ok": true, "cohort": cohortJSON(resp.Cohort)}
	if req.Op == "cohort-register" && len(resp.Cohort) == 1 {
		r := resp.Cohort[0]
		out["registered"], out["project"], out["agent"], out["session"] = true, r.Project, r.Agent, r.SessionID
		out["next_checkpoint_due"] = r.nextCheckpoint()
		if p, err := LoadCohortProjectAt(ctx, r.Root, r.Project, "refs/remotes/origin/main"); err == nil {
			out["gm"], out["term"] = p.GM.GM, p.GM.Term
		}
	}
	c.printJSON(out)
	return nil
}

func (r CohortReg) nextCheckpoint() time.Time { return r.checkpointAnchor().Add(checkpointInterval) }

// cohortJSON is the registrations as scripts see them, with the next
// checkpoint due time spelled out.
func cohortJSON(regs []CohortReg) []map[string]any {
	out := []map[string]any{}
	for _, r := range regs {
		out = append(out, map[string]any{
			"project": r.Project, "agent": r.Agent, "session": r.SessionID, "root": r.Root, "channel": r.Channel,
			"off_duty": r.OffDuty, "registered_at": r.RegisteredAt, "last_checkpoint": r.LastCheckpoint,
			"next_checkpoint_due": r.nextCheckpoint(),
		})
	}
	return out
}

// registration returns session's registration in project.
func (c *cli) registration(ctx context.Context, session, project string) (CohortReg, error) {
	resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "cohort-status", Cohort: &CohortReg{Project: project}})
	if err != nil {
		return CohortReg{}, errors.New("no slack-agent-chat listener is running")
	}
	for _, r := range resp.Cohort {
		if r.SessionID == session {
			return r, nil
		}
	}
	return CohortReg{}, fmt.Errorf("this session is not registered in %s", project)
}

// registeredIn reports whether session is registered in the cohort whose
// project channel is channel.
func (c *cli) registeredIn(ctx context.Context, session, channel string) bool {
	resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "cohort-status"})
	if err != nil {
		return false
	}
	for _, r := range resp.Cohort {
		if r.SessionID == session && r.Channel == channel {
			return true
		}
	}
	return false
}

// logCheckpoint appends a checkpoint's private context note to this home's
// local log, readable only by its owner. It never leaves the machine.
func (c *cli) logCheckpoint(project, drift, note string) error {
	line, err := json.Marshal(map[string]string{"at": time.Now().Format(time.RFC3339), "project": project,
		"drift": strings.TrimSpace(drift), "context": strings.TrimSpace(note)})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.home.StateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(c.home.StateDir, "checkpoints.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
