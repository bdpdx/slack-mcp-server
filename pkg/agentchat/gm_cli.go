package agentchat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/slack-go/slack"
)

// Exit statuses of `chat gm`, matching the claim outcomes.
const (
	exitGMLost        = 4 // lost, refused, or the caller no longer holds the term
	exitGMUnavailable = 5 // the authority could not be read or written
)

func (c *cli) gm(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gm status|claim|verify|release")
	}
	fs := flag.NewFlagSet("gm", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	project := fs.String("project", "", "project name")
	agent := fs.String("agent", "", "this agent's Slack name")
	root := fs.String("project-root", "", "the rezilient-project-state checkout (default: from the projects/ symlink)")
	expectTerm := fs.Int("expect-term", -1, "claim: the term the deadline notice named")
	expectGM := fs.String("expect-gm", "", "claim: the GM the deadline notice named")
	term := fs.Int("term", -1, "verify, release: the term this agent holds")
	claimID := fs.String("claim-id", "", "verify, release: this agent's claim ID")
	to := fs.String("to", "", "release: the agent to hand GM to")
	reason := fs.String("reason", "GM unavailable", "claim: why")
	userDirected := fs.Bool("user-directed", false, "claim: the user told this agent to take over; skips only the deadline check (registration, duty, liveness and holds still apply); say so in --reason")
	if _, err := parseArgs(fs, args[1:]); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("gm %s needs --project", args[0])
	}
	dir := *root
	if dir == "" {
		wd, _ := os.Getwd()
		var err error
		if dir, err = projectStateRoot(wd); err != nil {
			return exitError{exitGMUnavailable, err}
		}
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return exitError{exitGMUnavailable, err}
	}
	if err := checkProjectState(ctx, dir); err != nil {
		return exitError{exitGMUnavailable, err}
	}
	a := &GMAuthority{Root: dir, Project: *project, Agent: *agent}
	if args[0] != "status" && *agent == "" {
		return fmt.Errorf("gm %s needs --agent", args[0])
	}
	// Every gm operation is bounded: a stuck lock or network must never
	// become an unbounded stall.
	ctx, cancel := context.WithTimeout(ctx, gmCommandTimeout)
	defer cancel()
	if args[0] != "status" {
		// This home's bot announces the outcome and fences its own writes, so
		// it must be the agent named.
		me, err := c.identity(ctx)
		if err != nil {
			return exitError{exitGMUnavailable, err}
		}
		if me.agentName != *agent {
			return exitError{exitGMLost, fmt.Errorf("--agent %s is not this home's agent (%s)", *agent, me.agentName)}
		}
	}
	switch args[0] {
	case "status":
		st, err := a.Status(ctx)
		if err != nil {
			return exitError{exitGMUnavailable, err}
		}
		c.printJSON(st)
		return nil
	case "verify":
		if *term < 0 || *claimID == "" {
			return errors.New("gm verify needs --term and --claim-id")
		}
		ok, st, err := a.Verify(ctx, *term, *claimID)
		if err != nil {
			return exitError{exitGMUnavailable, err}
		}
		c.printJSON(map[string]any{"holds": ok, "state": st})
		if !ok {
			return exitError{exitGMLost, fmt.Errorf("%s no longer holds term %d; stop GM work", *agent, *term)}
		}
		return nil
	case "init":
		r, err := a.Init(ctx)
		if err != nil {
			return exitError{exitGMUnavailable, err}
		}
		c.printJSON(r)
		return c.claimExit(ctx, r, *project, fmt.Sprintf("GM term 1 (claim %s): %s is the first GM", r.State.ClaimID, *agent))
	case "claim":
		if *expectTerm < 0 || *expectGM == "" {
			return errors.New("gm claim needs --expect-term and --expect-gm from the notice")
		}
		if *userDirected && *reason == "GM unavailable" {
			return errors.New("a --user-directed claim needs --reason naming the user's instruction")
		}
		// Admission (registration, duty, fresh liveness, holds, the expected
		// authority and, unless user-directed, a due and still-unanswered
		// deadline) is rechecked after the lock and before every attempt.
		a.Admit = func(ctx context.Context) error {
			return c.claimEligible(ctx, *project, *agent, *expectTerm, *expectGM, *userDirected)
		}
		r, err := a.Claim(ctx, *expectTerm, *expectGM, *reason)
		if err != nil {
			return exitError{exitGMUnavailable, err}
		}
		c.printJSON(r)
		return c.claimExit(ctx, r, *project, fmt.Sprintf("ACTING GM term %d (claim %s): %s takes over from %s: %s",
			r.State.Term, r.State.ClaimID, *agent, *expectGM, *reason))
	case "release":
		if *term < 0 || *claimID == "" || *to == "" {
			return errors.New("gm release needs --term, --claim-id and --to")
		}
		r, err := a.Release(ctx, *term, *claimID, *to)
		c.printJSON(r)
		if err != nil && r.Outcome == "" {
			return exitError{exitGMUnavailable, err}
		}
		return c.claimExit(ctx, r, *project, fmt.Sprintf("GM term %d (claim %s): %s hands GM back to %s",
			r.State.Term, r.State.ClaimID, *agent, *to))
	}
	return fmt.Errorf("unknown gm command %q", args[0])
}

// gmCommandTimeout bounds each chat gm command.
const gmCommandTimeout = 2 * time.Minute

// claimEligible asks this home's listener whether this session may claim:
// registered as agent, on duty, not held, and a matching GM deadline due
// with its successor slot reached.
func (c *cli) claimEligible(ctx context.Context, project, agent string, term int, gm string, userDirected bool) error {
	sub, err := detectSession(os.Getenv)
	if err != nil {
		return err
	}
	resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "cohort-claim-check", SessionID: sub.SessionID,
		Cohort: &CohortReg{Project: project, Agent: agent}, Expect: &GMState{Term: term, GM: gm}, UserDirected: userDirected})
	if err != nil {
		return fmt.Errorf("%w: no slack-agent-chat listener is running", ErrAdmitUnavailable)
	}
	if !resp.OK {
		if resp.Unavailable {
			return fmt.Errorf("%w: %s", ErrAdmitUnavailable, resp.Error)
		}
		return fmt.Errorf("not eligible to claim: %s", resp.Error)
	}
	return nil
}

// claimExit maps an outcome to the exit status, announcing a win in the
// project channel (the receipt is already on the remote).
func (c *cli) claimExit(ctx context.Context, r ClaimResult, project, announce string) error {
	switch r.Outcome {
	case ClaimWon:
		channel, _, err := c.resolveChannel(ctx, project)
		if err == nil {
			_, _, err = c.bot.PostMessageContext(ctx, channel, slack.MsgOptionText(slackEscaper.Replace(announce), false))
		}
		if err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: the claim holds, but announcing it in #%s failed: %v\n", project, err)
		}
		return nil
	case ClaimUnavailable:
		return exitError{exitGMUnavailable, errors.New(r.Reason)}
	default:
		return exitError{exitGMLost, fmt.Errorf("%s: %s", r.Outcome, r.Reason)}
	}
}
