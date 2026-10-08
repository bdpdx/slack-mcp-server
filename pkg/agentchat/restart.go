package agentchat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/version"
)

// shutdownDelay lets the listener write its reply to a shutdown request
// before it stops; stopWait bounds how long restart waits for the old
// listener to exit.
const (
	shutdownDelay = 200 * time.Millisecond
	stopWait      = 15 * time.Second
)

// listenerCmd runs `chat listener restart|stop`. Restart replaces the
// home's running listener with this binary's, so an upgrade takes effect
// without stopping any watch: the new listener restores every session's
// watches, cohort registrations and delivery marks from the state file, and
// waiting approval hooks register their requests with it again.
func (c *cli) listenerCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "restart" && args[0] != "stop") {
		return errors.New("usage: listener restart [--if-running] | listener stop")
	}
	op := args[0]
	fs := flag.NewFlagSet("listener "+op, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	ifRunning := fs.Bool("if-running", false, "restart only a listener that is running; otherwise do nothing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	before, running := c.listenerVersion(ctx)
	if running {
		if err := c.stopListener(ctx); err != nil {
			return err
		}
	}
	out := map[string]any{"ok": true, "home": c.home.Dir, "was_running": running, "previous_version": before}
	if op == "stop" || (!running && *ifRunning) {
		c.printJSON(out)
		return nil
	}
	if err := c.ensureListener(ctx); err != nil {
		return err
	}
	after, _ := c.listenerVersion(ctx)
	out["running"], out["version"], out["binary_version"] = true, after, version.Version
	c.printJSON(out)
	return nil
}

// listenerVersion reports whether the home's listener answers, and its
// build ("unknown" for one too old to say).
func (c *cli) listenerVersion(ctx context.Context) (string, bool) {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := SendControl(sctx, c.home.ControlSocket, ControlRequest{Op: "status"})
	if listenerDown(err) {
		return "", false
	}
	if resp.Version == "" {
		return "unknown", true
	}
	return resp.Version, true
}

// stopListener asks the running listener to shut down and waits until it
// has exited: its socket no longer answers and its lock is free, so a new
// listener can take over.
func (c *cli) stopListener(ctx context.Context) error {
	_, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "shutdown"})
	var refused *RefusedError
	switch {
	case errors.As(err, &refused) && strings.HasPrefix(refused.Reason, "unknown op"):
		// A listener older than the shutdown op: stop it the way its own
		// signal handling expects, SIGTERM, which it handles gracefully.
		if terr := c.signalListener(); terr != nil {
			return fmt.Errorf("the running listener predates in-place restart, and stopping it failed: %w", terr)
		}
	case err != nil && !listenerDown(err):
		return fmt.Errorf("asking the listener to shut down: %w", err)
	}
	lock := filepath.Join(c.home.StateDir, "listener.lock")
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		if _, up := c.listenerVersion(ctx); !up {
			if release, err := AcquireListenerLock(lock); err == nil {
				release()
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the listener did not exit within %s; see %s", stopWait, c.home.LogFile)
}

// signalListener sends SIGTERM to this home's listener process, found by
// its command line (`… chat --env-file ENV listen`, as ensureListener
// starts it).
func (c *cli) signalListener() error {
	pattern := "chat --env-file " + regexp.QuoteMeta(c.home.EnvFile) + " listen$"
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		return fmt.Errorf("finding the listener process: %w", err)
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return errors.New("no listener process found")
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			return fmt.Errorf("stopping listener process %d: %w", pid, err)
		}
	}
	return nil
}
