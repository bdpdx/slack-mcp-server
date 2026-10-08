package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

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
		fmt.Fprintf(w, "  %s: ", h.Path) // before the restart, which can take a while
		out, err := r.Run(nil, bin, "chat", "--env-file", EnvPath(h.Path), "listener", "restart", "--if-running")
		failed = failed || err != nil
		fmt.Fprintln(w, restartLine(out, err))
	}
	return failed
}

// restartLine describes one home's `listener restart --if-running` result.
func restartLine(out string, err error) string {
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
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

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
