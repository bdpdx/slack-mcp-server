package agentchat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/version"
)

// Restart timing. shutdownDelay lets the listener write its reply to a
// shutdown request before it stops; answerDrain bounds how long it first
// waits for hooks to collect answers already given; shutdownTimeout bounds
// the shutdown request itself; stopWait bounds the wait for the old
// listener to exit; probeTimeout bounds one status probe.
var (
	shutdownDelay   = 200 * time.Millisecond
	answerDrain     = 5 * time.Second
	shutdownTimeout = 10 * time.Second
	stopWait        = 15 * time.Second
	probeTimeout    = 5 * time.Second
)

// restartAttempts bounds how often restart tries again when the new
// listener fails to start, or some other listener (another session's watch
// command, run from an older binary) takes the home in the gap between
// stopping the old listener and starting this one.
const restartAttempts = 3

// listenerCmd runs `chat listener restart|stop`. Restart replaces the
// home's running listener with this binary's, so an upgrade takes effect
// without stopping any watch: the new listener restores every session's
// watches, cohort registrations and delivery marks from the state file, and
// waiting approval hooks register their requests with it again.
func (c *cli) listenerCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "restart" && args[0] != "stop") {
		return errors.New("usage: listener restart [--if-running] [--force] | listener stop")
	}
	op := args[0]
	fs := flag.NewFlagSet("listener "+op, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	ifRunning := fs.Bool("if-running", false, "restart only a listener that is running; otherwise do nothing")
	force := fs.Bool("force", false, "restart even a listener already running this binary's build")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	before, running := c.listenerState(ctx)
	out := map[string]any{"ok": true, "home": c.home.Dir, "was_running": running, "previous_version": before, "binary_version": version.Version}
	switch {
	case op == "stop":
		if running {
			if err := c.stopListener(ctx); err != nil {
				return err
			}
		}
		c.printJSON(out)
		return nil
	case !running && *ifRunning:
		c.printJSON(out)
		return nil
	case running && before == version.Version && !*force:
		out["running"], out["version"], out["already_current"] = true, before, true
		c.printJSON(out)
		return nil
	}
	after, err := c.replaceListener(ctx)
	if err != nil {
		return err
	}
	out["running"], out["version"] = true, after
	c.printJSON(out)
	return nil
}

// replaceListener stops whatever listener runs for the home and starts this
// binary's, and succeeds only once the home's listener answers with this
// binary's own build.
func (c *cli) replaceListener(ctx context.Context) (string, error) {
	return replaceWith(
		func() (string, bool) { return c.listenerState(ctx) },
		func() error { return c.stopListener(ctx) },
		func() error { return c.ensureListener(ctx) },
		version.Version, c.recovery())
}

// recovery is the command that starts this home's listener by hand.
func (c *cli) recovery() string {
	return fmt.Sprintf("slack-mcp-server chat --env-file %s listener restart (log: %s)", c.home.EnvFile, c.home.LogFile)
}

// replaceWith is replaceListener's protocol: probe reports the running
// listener's build, stop and start replace it, want is the build that must
// answer at the end. A start that fails is tried again.
func replaceWith(probe func() (string, bool), stop, start func() error, want, recovery string) (string, error) {
	problem := ""
	for i := 0; i < restartAttempts; i++ {
		if _, up := probe(); up {
			if err := stop(); err != nil {
				return "", err
			}
		}
		if err := start(); err != nil {
			problem = "the new listener did not start: " + err.Error()
			continue
		}
		v, up := probe()
		if up && v == want {
			return v, nil
		}
		problem = fmt.Sprintf("the home's listener is %s, not this binary's %s", v, want)
		if !up {
			problem = "the new listener exited right after starting"
		}
	}
	return "", fmt.Errorf("%s, after %d attempts; this home may have no listener now: start it with %s", problem, restartAttempts, recovery)
}

// listenerState reports whether the home's listener is running, and its
// build: "unknown" for one too old to say, "unresponsive" for one whose
// socket accepts connections but does not answer in time. Only a missing
// socket, or one that refuses connections, counts as not running.
func (c *cli) listenerState(ctx context.Context) (string, bool) {
	for i := 0; i < 3; i++ {
		sctx, cancel := context.WithTimeout(ctx, probeTimeout)
		resp, err := SendControl(sctx, c.home.ControlSocket, ControlRequest{Op: "status"})
		cancel()
		var refused *RefusedError
		switch {
		case err == nil && resp.Version != "":
			return resp.Version, true
		case err == nil, errors.As(err, &refused):
			return "unknown", true
		case noListener(err):
			return "", false
		}
	}
	return "unresponsive", true
}

// noListener reports whether a control-socket error means nothing listens:
// no socket file, or a socket that refuses connections.
func noListener(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// stopListener asks the running listener to shut down and waits until it
// has exited: nothing accepts connections on its socket and its lock is
// free, so a new listener can take over. A listener that predates the
// shutdown op, or does not answer, is sent SIGTERM, which it handles
// gracefully.
func (c *cli) stopListener(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	_, err := SendControl(sctx, c.home.ControlSocket, ControlRequest{Op: "shutdown"})
	cancel()
	signaled := false
	var refused *RefusedError
	switch {
	case err == nil, noListener(err):
	case errors.As(err, &refused) && !strings.HasPrefix(refused.Reason, "unknown op"):
		return fmt.Errorf("the listener refused to shut down: %w", err)
	default: // too old for the op, or not answering
		if serr := c.terminate(); serr != nil {
			return fmt.Errorf("stopping the listener: %w", serr)
		}
		signaled = true
	}
	if c.waitStopped(ctx) {
		return nil
	}
	if !signaled {
		if serr := c.terminate(); serr == nil && c.waitStopped(ctx) {
			return nil
		}
	}
	return fmt.Errorf("the listener did not exit within %s; see %s", stopWait, c.home.LogFile)
}

// waitStopped waits up to stopWait for the home's listener to be gone.
func (c *cli) waitStopped(ctx context.Context) bool {
	lock := filepath.Join(c.home.StateDir, "listener.lock")
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		conn, err := net.DialTimeout("unix", c.home.ControlSocket, time.Second)
		if err == nil {
			conn.Close()
		} else if noListener(err) {
			if release, err := AcquireListenerLock(lock); err == nil {
				release()
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// terminate sends SIGTERM to the home's listener (c.kill in tests).
func (c *cli) terminate() error {
	if c.kill != nil {
		return c.kill()
	}
	return c.signalListener()
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
