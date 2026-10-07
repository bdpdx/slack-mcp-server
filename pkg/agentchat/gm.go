package agentchat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// GM authority is projects/<project>/gm.json on the project-state repo's
// origin/main. Listeners run per home and per machine, so only that shared
// ref can arbitrate; a non-force push of a commit whose single parent is the
// tip it was validated against is the compare-and-swap. Claims are built
// with git plumbing in a private index, never in the shared working branch,
// and never rebased over another claim.

// ClaimOutcome is how a claim (or hand-back) ended.
type ClaimOutcome string

const (
	ClaimWon         ClaimOutcome = "won"         // the remote shows our term and claim ID
	ClaimLost        ClaimOutcome = "lost"        // the term or GM had already changed
	ClaimRefused     ClaimOutcome = "refused"     // not eligible: held, not on the roster, or already GM
	ClaimUnavailable ClaimOutcome = "unavailable" // the authority could not be read or written
)

// ClaimResult reports a claim and the GM state it found or made.
type ClaimResult struct {
	Outcome ClaimOutcome `json:"outcome"`
	State   GMState      `json:"state"`
	Reason  string       `json:"reason,omitempty"`
}

var (
	errUncertain = errors.New("push result uncertain")
	errRejected  = errors.New("push rejected") // main moved: not a fast-forward
	errDeclined  = errors.New("push declined") // a hook or branch rule refused it
)

const claimAttempts = 5

// GMAuthority reads and changes one project's GM authority from a
// project-state checkout.
type GMAuthority struct {
	Root    string // a rezilient-project-state checkout
	Project string
	Agent   string // the caller
	Env     []string

	beforePush func()            // tests: runs between building and pushing
	pushErr    func(error) error // tests: rewrites the push result
}

func (a *GMAuthority) git(ctx context.Context, env []string, stdin []byte, args ...string) (string, error) {
	return gitIn(ctx, a.Root, append(append([]string{}, a.Env...), env...), stdin, args...)
}

// lock serializes this checkout's claim operations, across processes and
// homes that share the clone.
func (a *GMAuthority) lock(ctx context.Context) (func(), error) {
	dir, err := a.git(ctx, nil, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.Root, dir)
	}
	f, err := os.OpenFile(filepath.Join(dir, "rezilient-gm.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Poll a non-blocking lock so ctx can cancel the wait: a stuck holder
	// must never recreate an unbounded stall.
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("waiting for the claim lock: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// fetchTip fetches origin/main and returns its commit.
func (a *GMAuthority) fetchTip(ctx context.Context) (string, error) {
	if _, err := a.git(ctx, nil, nil, "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return "", err
	}
	return a.git(ctx, nil, nil, "rev-parse", "refs/remotes/origin/main^{commit}")
}

// at reads the project as of commit: succession order, GM state, holds.
func (a *GMAuthority) at(ctx context.Context, commit string) (*CohortProject, error) {
	return LoadCohortProjectAt(ctx, a.Root, a.Project, commit)
}

// Status returns the current GM state on the remote.
func (a *GMAuthority) Status(ctx context.Context) (GMState, error) {
	if !projectName.MatchString(a.Project) {
		return GMState{}, fmt.Errorf("%q is not a project name", a.Project)
	}
	tip, err := a.fetchTip(ctx)
	if err != nil {
		return GMState{}, err
	}
	p, err := a.at(ctx, tip)
	if err != nil {
		return GMState{}, err
	}
	return p.GM, nil
}

// commitState builds, without touching the working tree or the shared
// index, a commit whose only parent is parent and which changes only
// gm.json to st.
func (a *GMAuthority) commitState(ctx context.Context, parent string, st GMState, msg string) (string, error) {
	idx, err := os.CreateTemp("", "rezilient-gm-index-")
	if err != nil {
		return "", err
	}
	idx.Close()
	defer os.Remove(idx.Name())
	env := []string{"GIT_INDEX_FILE=" + idx.Name()}
	if _, err := a.git(ctx, env, nil, "read-tree", parent); err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	blob, err := a.git(ctx, env, append(data, '\n'), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := a.git(ctx, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+a.Project+"/gm.json"); err != nil {
		return "", err
	}
	tree, err := a.git(ctx, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return a.git(ctx, env, nil, "commit-tree", tree, "-p", parent, "-m", msg)
}

// push publishes commit as main without force.
func (a *GMAuthority) push(ctx context.Context, commit string) error {
	_, err := a.git(ctx, nil, nil, "push", "-q", "origin", commit+":refs/heads/main")
	if err != nil {
		s := err.Error()
		if strings.Contains(s, "remote rejected") || strings.Contains(s, "declined") {
			err = fmt.Errorf("%w: %v", errDeclined, err)
		} else if strings.Contains(s, "rejected") || strings.Contains(s, "non-fast-forward") || strings.Contains(s, "fetch first") {
			err = errRejected
		} else {
			err = fmt.Errorf("%w: %v", errUncertain, err)
		}
	}
	if a.pushErr != nil {
		err = a.pushErr(err)
	}
	return err
}

// receipt reports whether the remote now carries our claim: its gm.json has
// our claim ID and our commit is on main.
func (a *GMAuthority) receipt(ctx context.Context, commit string, want GMState) (bool, GMState, error) {
	tip, err := a.fetchTip(ctx)
	if err != nil {
		return false, GMState{}, err
	}
	p, err := a.at(ctx, tip)
	if err != nil {
		return false, GMState{}, err
	}
	if p.GM.ClaimID != want.ClaimID || p.GM.Term != want.Term || p.GM.GM != want.GM {
		return false, p.GM, nil // the exact term, GM and claim ID, not just the ID
	}
	if commit != "" {
		if _, err := a.git(ctx, nil, nil, "merge-base", "--is-ancestor", commit, tip); err != nil {
			return false, p.GM, nil
		}
	}
	return true, p.GM, nil
}

func newClaimID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// transition runs one guarded gm.json change: valid(p) checks the state at
// the captured tip (returning a non-empty outcome to stop) and next builds
// the new state from it.
func (a *GMAuthority) transition(ctx context.Context, valid func(*CohortProject) (ClaimOutcome, string), next func(*CohortProject, string) GMState, msg string) (ClaimResult, error) {
	if !projectName.MatchString(a.Project) {
		return ClaimResult{}, fmt.Errorf("%q is not a project name", a.Project)
	}
	unlock, err := a.lock(ctx)
	if err != nil {
		return ClaimResult{Outcome: ClaimUnavailable, Reason: err.Error()}, nil
	}
	defer unlock()
	minted := map[string]GMState{} // every state this call has tried to publish, by claim ID
	var declined error             // the last push a hook or branch rule refused
	for attempt := 0; attempt < claimAttempts; attempt++ {
		tip, err := a.fetchTip(ctx)
		if err != nil {
			return ClaimResult{Outcome: ClaimUnavailable, Reason: err.Error()}, nil
		}
		p, err := a.at(ctx, tip)
		if err != nil {
			return ClaimResult{Outcome: ClaimUnavailable, Reason: err.Error()}, nil
		}
		// A push this call gave up on may have landed after all: the
		// remote carrying one of our claim IDs is a win, never a loss and
		// never a reason to mint another term.
		if st, ok := minted[p.GM.ClaimID]; ok && st == p.GM {
			return ClaimResult{Outcome: ClaimWon, State: p.GM}, nil
		}
		if outcome, reason := valid(p); outcome != "" {
			return ClaimResult{Outcome: outcome, State: p.GM, Reason: reason}, nil
		}
		claimID := newClaimID()
		st := next(p, claimID)
		minted[claimID] = st
		commit, err := a.commitState(ctx, tip, st, fmt.Sprintf("%s: GM term %d: %s (%s)", a.Project, st.Term, st.GM, msg))
		if err != nil {
			return ClaimResult{}, err
		}
		if a.beforePush != nil {
			a.beforePush()
		}
		perr := a.push(ctx, commit)
		switch {
		case perr == nil, errors.Is(perr, errUncertain):
			ok, cur, err := a.receipt(ctx, commit, st)
			if err != nil {
				return ClaimResult{Outcome: ClaimUnavailable, State: st, Reason: "push result unknown and the remote is unreadable: " + err.Error()}, nil
			}
			if ok {
				return ClaimResult{Outcome: ClaimWon, State: cur}, nil
			}
			if perr == nil {
				return ClaimResult{Outcome: ClaimLost, State: cur, Reason: "pushed, but the remote no longer carries this claim"}, nil
			}
			// The remote shows no trace of the uncertain push: it did not land.
		case errors.Is(perr, errRejected):
			// main moved: re-fetch and revalidate; a changed term ends it there.
		case errors.Is(perr, errDeclined):
			// A hook or branch rule refused it. Retry: if it landed after
			// all, the next attempt finds our claim ID on the remote.
			declined = perr
		default:
			return ClaimResult{}, perr
		}
	}
	if declined != nil {
		return ClaimResult{Outcome: ClaimUnavailable, Reason: "the remote declined the push: " + declined.Error()}, nil
	}
	return ClaimResult{Outcome: ClaimUnavailable, Reason: "main kept moving; gave up after retries"}, nil
}

// Claim takes over as GM, expecting the current authority to be
// (expectTerm, expectGM): the GM the deadline notice named.
func (a *GMAuthority) Claim(ctx context.Context, expectTerm int, expectGM, reason string) (ClaimResult, error) {
	return a.transition(ctx, func(p *CohortProject) (ClaimOutcome, string) {
		if p.GM.Term != expectTerm || p.GM.GM != expectGM {
			return ClaimLost, fmt.Sprintf("the authority is term %d (%s), not the expected term %d (%s)", p.GM.Term, p.GM.GM, expectTerm, expectGM)
		}
		switch {
		case !p.InRoster(a.Agent):
			return ClaimRefused, a.Agent + " is not in the succession order"
		case p.Held(a.Agent):
			return ClaimRefused, a.Agent + " is on hold"
		case p.GM.GM == a.Agent:
			return ClaimRefused, a.Agent + " is already GM"
		}
		return "", ""
	}, func(p *CohortProject, id string) GMState {
		return GMState{Term: p.GM.Term + 1, GM: a.Agent, ClaimID: id, Since: time.Now().Format(time.RFC3339), Reason: reason}
	}, "claim: "+reason)
}

// Init publishes term 1 for a project that has no gm.json yet, so its first
// GM (the first in the succession order) holds a claim ID to verify GM-file
// writes with. Only that agent can, and only once.
func (a *GMAuthority) Init(ctx context.Context) (ClaimResult, error) {
	return a.transition(ctx, func(p *CohortProject) (ClaimOutcome, string) {
		if p.GM.Term != 0 || p.GM.ClaimID != "" {
			return ClaimLost, fmt.Sprintf("the project already has term %d (%s)", p.GM.Term, p.GM.GM)
		}
		if p.GM.GM != a.Agent {
			return ClaimRefused, fmt.Sprintf("%s is the project's first GM, not %s", p.GM.GM, a.Agent)
		}
		if p.Held(a.Agent) {
			return ClaimRefused, a.Agent + " is on hold"
		}
		return "", ""
	}, func(p *CohortProject, id string) GMState {
		return GMState{Term: 1, GM: a.Agent, ClaimID: id, Since: time.Now().Format(time.RFC3339), Reason: "first GM"}
	}, "init")
}

// Release hands GM authority to another agent. Only the current GM, naming
// its term and claim ID, can.
func (a *GMAuthority) Release(ctx context.Context, term int, claimID, to string) (ClaimResult, error) {
	r, err := a.transition(ctx, func(p *CohortProject) (ClaimOutcome, string) {
		if p.GM.Term != term || p.GM.ClaimID != claimID || p.GM.GM != a.Agent {
			return ClaimLost, fmt.Sprintf("%s does not hold term %d with that claim ID", a.Agent, term)
		}
		if !p.InRoster(to) || p.Held(to) {
			return ClaimRefused, to + " is not an eligible GM"
		}
		if to == a.Agent {
			return ClaimRefused, a.Agent + " already holds GM"
		}
		return "", ""
	}, func(p *CohortProject, id string) GMState {
		return GMState{Term: p.GM.Term + 1, GM: to, ClaimID: id, Since: time.Now().Format(time.RFC3339),
			Reason: "hand-back", HandoffOf: fmt.Sprintf("term %d claim %s", p.GM.Term, p.GM.ClaimID)}
	}, "hand-back to "+to)
	if err == nil && r.Outcome != ClaimWon {
		err = fmt.Errorf("hand-back %s: %s", r.Outcome, r.Reason)
	}
	return r, err
}

// Verify reports whether the caller still holds term with claimID on the
// remote. The GM runs it before every GM-file publication.
func (a *GMAuthority) Verify(ctx context.Context, term int, claimID string) (bool, GMState, error) {
	st, err := a.Status(ctx)
	if err != nil {
		return false, GMState{}, err
	}
	return st.Term == term && st.ClaimID == claimID && st.GM == a.Agent, st, nil
}
