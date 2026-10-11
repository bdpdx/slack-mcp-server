package agentchat

import (
	"context"

	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Pin pins sub to its agent process and seat (CS-42).
//
// Claude: the inbox socket is named after the claude process
// (/tmp/cc-socks/<pid>.sock), which ends when the session does.
//
// Codex: the app-server runs the thread and outlives its terminal, so the
// pin is the `codex --remote … -C <dir>` terminal for the thread's working
// directory, which is also its seat. A sub-agent's thread may not watch:
// it would take over its parent's seat. Finding the terminal depends on its
// command line; when none is found the watch still starts, unpinned, with a
// warning.
func (d *HostDeliverer) Pin(ctx context.Context, sub *Subscription) (string, error) {
	switch sub.Kind {
	case KindClaude:
		pid := pidFromSocket(sub.Socket)
		if pid == 0 {
			return "watch not pinned to its Claude process (unrecognized inbox socket " + sub.Socket + "); it ends when the session's socket goes away", nil
		}
		start, ok := procStart(pid)
		if !ok {
			return "", fmt.Errorf("this session's Claude process (%d) is not running", pid)
		}
		sub.PinPID, sub.PinStart = pid, start
		return "", nil
	case KindCodex:
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cwd, parent, err := CodexThreadInfo(ctx, d.CodexSocket, sub.ThreadID)
		if err != nil {
			return "watch not pinned to a Codex terminal: reading the thread failed: " + err.Error(), nil
		}
		if parent != "" {
			return "", fmt.Errorf("a sub-agent thread (parent %s) cannot watch Slack; the top-level agent watches", parent)
		}
		sub.Seat = canonicalDir(cwd)
		rows, err := listProcs()
		if err == nil {
			if tui, ok := findCodexTUI(rows, sub.Seat); ok {
				sub.PinPID, sub.PinStart = tui.PID, tui.Start
				return "", nil
			}
		}
		return "watch not pinned to a Codex terminal (no `codex --remote -C " + sub.Seat + "` process found): it ends when a new thread watches from this directory or the app-server unloads this one", nil
	}
	return "", nil
}

var socketPID = regexp.MustCompile(`^(\d+)\.sock$`)

// pidFromSocket returns the claude PID a Claude inbox socket is named
// after, or 0.
func pidFromSocket(socket string) int {
	m := socketPID.FindStringSubmatch(filepath.Base(socket))
	if m == nil {
		return 0
	}
	pid, _ := strconv.Atoi(m[1])
	return pid
}

// procRow is one running process.
type procRow struct {
	PID     int
	Start   int64 // procStart's value
	Command string
}

// listProcs lists the running codex processes with their start times.
func listProcs() ([]procRow, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var rows []procRow
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || filepath.Base(f[1]) != "codex" {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		if start, ok := procStart(pid); ok {
			rows = append(rows, procRow{PID: pid, Start: start, Command: strings.Join(f[1:], " ")})
		}
	}
	return rows, nil
}

// findCodexTUI picks the Codex terminal working in dir: the newest
// `codex --remote` process whose -C/--cd is dir.
func findCodexTUI(rows []procRow, dir string) (procRow, bool) {
	var best procRow
	found := false
	for _, r := range rows {
		f := strings.Fields(r.Command)
		if len(f) == 0 || filepath.Base(f[0]) != "codex" || !slicesHasPrefix(f[1:], "--remote") {
			continue
		}
		if cd := flagValue(f[1:], "-C", "--cd"); cd == "" || canonicalDir(cd) != canonicalDir(dir) {
			continue
		}
		if !found || r.Start > best.Start {
			best, found = r, true
		}
	}
	return best, found
}

func slicesHasPrefix(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// flagValue returns the value of the first of names in args (`-C dir` or
// `-C=dir`), or "".
func flagValue(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, n+"="); ok {
				return v
			}
		}
	}
	return ""
}

// canonicalDir cleans dir and resolves symlinks where it can.
func canonicalDir(dir string) string {
	if dir == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// errPinnedAgentGone marks a session whose pinned agent process exited.
var errPinnedAgentGone = fmt.Errorf("%w: its agent process exited", ErrSessionGone)

// pinnedAgentGone reports whether sub's pinned agent process has exited.
func pinnedAgentGone(sub *Subscription) bool {
	return sub.PinPID > 0 && !procAlive(sub.PinPID, sub.PinStart)
}
