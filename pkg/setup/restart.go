package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// StreamRunner runs a command with its stderr streamed to w as it runs,
// returning only its stdout.
type StreamRunner interface {
	RunStream(w io.Writer, env []string, name string, args ...string) (string, error)
}

// RunStream implements StreamRunner.
func (ExecRunner) RunStream(w io.Writer, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = commandEnv(os.Environ(), env)
	cmd.Stderr = w
	out, err := cmd.Output()
	return string(out), err
}

// RestartListeners replaces the running listener of every home that has an
// env file with the binary at bin (just installed), so an upgrade takes
// effect without stopping any watch or restarting agent sessions. Homes with
// no running listener, or one already on this build, are left alone. It
// reports whether any restart failed.
func RestartListeners(w io.Writer, r Runner, bin string, homes []Home) (failed bool) {
	header := false
	for _, h := range homes {
		if !h.HasEnv {
			continue
		}
		if !header {
			fmt.Fprintln(w, "\nListeners")
			header = true
		}
		args := []string{"chat", "--env-file", EnvPath(h.Path), "listener", "restart", "--if-running"}
		var out string
		var err error
		if sr, ok := r.(StreamRunner); ok {
			fmt.Fprintf(w, "  %s:\n", h.Path) // its progress follows as it runs
			out, err = sr.RunStream(&indenter{w: w, prefix: "    "}, nil, bin, args...)
			fmt.Fprintf(w, "    %s\n", restartLine(out, err))
		} else {
			out, err = r.Run(nil, bin, args...)
			fmt.Fprintf(w, "  %s: %s\n", h.Path, restartLine(out, err))
		}
		failed = failed || err != nil
	}
	return failed
}

// restartLine describes one home's `listener restart --if-running` result.
func restartLine(out string, err error) string {
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error() + " (the reason is printed above)"
		}
		return "restart FAILED: " + msg
	}
	var res struct {
		WasRunning bool   `json:"was_running"`
		Previous   string `json:"previous_version"`
		Version    string `json:"version"`
		Current    bool   `json:"already_current"`
	}
	if json.Unmarshal([]byte(lastLine(out)), &res) != nil {
		return "restarted (result unreadable: " + strings.TrimSpace(out) + ")"
	}
	if !res.WasRunning {
		return "not running; the next watch starts the new one"
	}
	if res.Current {
		return "already running this build (" + res.Version + ")"
	}
	return fmt.Sprintf("restarted on the new binary (%s → %s); watches carried over", res.Previous, res.Version)
}

// indenter prefixes every line written through it.
type indenter struct {
	w      io.Writer
	prefix string
	mid    bool // inside a line
}

func (in *indenter) Write(p []byte) (int, error) {
	var b []byte
	for _, c := range p {
		if !in.mid {
			b = append(b, in.prefix...)
			in.mid = true
		}
		b = append(b, c)
		if c == '\n' {
			in.mid = false
		}
	}
	if _, err := in.w.Write(b); err != nil {
		return 0, err
	}
	return len(p), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
