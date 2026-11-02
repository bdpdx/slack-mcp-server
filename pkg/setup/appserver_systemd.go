package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// appServerUnitName is the systemd unit name of a Codex home's app-server.
func appServerUnitName(home string) string { return appServerLabel(home) + ".service" }

// appServerUnitPath is where the user unit for a Codex home is installed.
func appServerUnitPath(userHome, home string) string {
	return filepath.Join(userHome, ".config", "systemd", "user", appServerUnitName(home))
}

// systemctl runs systemctl against the user's service manager.
func systemctl(r Runner, args ...string) (string, error) {
	return r.Run(nil, "systemctl", append([]string{"--user"}, args...)...)
}

// unitSpecifiers escapes the % that systemd expands in most unit settings.
func unitSpecifiers(v string) string { return strings.ReplaceAll(v, "%", "%%") }

// unitQuote renders v as one double-quoted word of a unit setting.
func unitQuote(v string) string {
	v = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v)
	return `"` + unitSpecifiers(v) + `"`
}

// renderAppServerUnit renders the systemd user unit that runs the supervisor
// with codexBin for one Codex home.
//
// KillMode=process signals the supervisor alone: it stops the daemon itself,
// and listeners started from Codex sessions share this cgroup and must outlive
// a restart. StartLimitIntervalSec=0 keeps systemd retrying a failing service.
func renderAppServerUnit(home, userHome, supervisor, codexBin, username string) []byte {
	path := userHome + "/.local/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	logs := filepath.Join(home, "app-server-daemon")
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Codex app-server for " + unitSpecifiers(home) + "\nStartLimitIntervalSec=0\n\n[Service]\nType=simple\n")
	for _, kv := range [][2]string{{"CODEX_HOME", home}, {"HOME", userHome}, {"LOGNAME", username}, {"PATH", path}, {"USER", username}} {
		b.WriteString("Environment=" + unitQuote(kv[0]+"="+kv[1]) + "\n")
	}
	exec := func(v string) string { return strings.ReplaceAll(unitQuote(v), "$", "$$") }
	b.WriteString("ExecStart=" + exec(supervisor) + " " + exec(codexBin) + "\n")
	b.WriteString("Restart=on-failure\nRestartSec=10\nKillMode=process\nTimeoutStopSec=30\n")
	b.WriteString("StandardOutput=append:" + unitSpecifiers(filepath.Join(logs, "systemd.stdout.log")) + "\n")
	b.WriteString("StandardError=append:" + unitSpecifiers(filepath.Join(logs, "systemd.stderr.log")) + "\n")
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

var (
	unitEnvRe  = regexp.MustCompile(`(?m)^Environment=(.*)$`)
	unitExecRe = regexp.MustCompile(`(?m)^ExecStart=(.*)$`)
	unitWordRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|(\S+)`)
	unitEscRe  = regexp.MustCompile(`\\(.)`)
)

// unitWords splits a unit setting's value into its unquoted words.
func unitWords(v string) []string {
	var words []string
	for _, m := range unitWordRe.FindAllStringSubmatch(v, -1) {
		w := m[2]
		if m[2] == "" {
			w = unitEscRe.ReplaceAllString(m[1], "$1")
		}
		words = append(words, strings.NewReplacer("%%", "%", "$$", "$").Replace(w))
	}
	return words
}

// unitCodexHome returns the CODEX_HOME a unit sets, if any.
func unitCodexHome(text string) (string, bool) {
	for _, line := range unitEnvRe.FindAllStringSubmatch(text, -1) {
		for _, w := range unitWords(line[1]) {
			if v, ok := strings.CutPrefix(w, "CODEX_HOME="); ok {
				return v, true
			}
		}
	}
	return "", false
}

// verifyAppServerUnit returns one problem per mismatch with what this
// installer would have written.
func verifyAppServerUnit(text, home, userHome string) []string {
	var problems []string
	v, ok := unitCodexHome(text)
	switch {
	case ok && filepath.Clean(v) != filepath.Clean(home):
		problems = append(problems, fmt.Sprintf("CODEX_HOME is %q, expected %q", v, home))
	case !ok && filepath.Clean(home) != filepath.Join(userHome, ".codex"):
		problems = append(problems, fmt.Sprintf("CODEX_HOME is not set, expected %q", home))
	}
	var args []string
	if m := unitExecRe.FindStringSubmatch(text); m != nil {
		args = unitWords(m[1])
	}
	if len(args) == 0 {
		problems = append(problems, "no program in ExecStart")
	} else if _, err := os.Stat(args[0]); err != nil {
		problems = append(problems, fmt.Sprintf("program %s does not exist", args[0]))
	}
	// A unit without the codex argument still works: the supervisor uses codex on PATH.
	if len(args) > 1 {
		if _, err := os.Stat(args[1]); err != nil {
			problems = append(problems, fmt.Sprintf("codex %s does not exist", args[1]))
		}
	}
	return problems
}

// ensureLinger keeps the user's service manager, and so the app-server,
// running with nobody logged in. Without it systemd stops them at logout.
func ensureLinger(r Runner, username string) (changed, notes []string) {
	if out, err := r.Run(nil, "loginctl", "show-user", username, "--property=Linger", "--value"); err == nil && strings.TrimSpace(out) == "yes" {
		return nil, nil
	}
	if out, err := r.Run(nil, "loginctl", "enable-linger", username); err != nil {
		return nil, []string{fmt.Sprintf("could not enable lingering (%s); the app-server stops when you log out until you run: sudo loginctl enable-linger %s", strings.TrimSpace(out+" "+err.Error()), username)}
	}
	return []string{"lingering enabled for " + username + ", so user services keep running after logout"}, nil
}

// ensureAppServerSystemd sets up (or verifies) the systemd user service that
// runs the Codex app-server for one home, and lingering for any home that has one.
func ensureAppServerSystemd(home, userHome string, r Runner, p Prompter) (changed, notes []string, err error) {
	changed, notes, err = ensureAppServerUnit(home, userHome, r, p)
	if err != nil || !exists(appServerUnitPath(userHome, home)) {
		return changed, notes, err
	}
	c, n := ensureLinger(r, currentUsername())
	return append(changed, c...), append(notes, n...), nil
}

// ensureAppServerUnit creates and starts a missing unit; an existing one is
// verified and never rewritten.
func ensureAppServerUnit(home, userHome string, r Runner, p Prompter) (changed, notes []string, err error) {
	label := appServerLabel(home)
	unit := appServerUnitName(home)
	path := appServerUnitPath(userHome, home)

	existing, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, nil, readErr
	}
	if readErr != nil {
		codex, err := r.LookPath("codex")
		if err != nil {
			return nil, []string{"Codex CLI not found; install Codex, then run ./install.sh again to set up its app-server"}, nil
		}
		// An older supervisor hardcodes its codex path and ignores the unit's,
		// so replace any supervisor that differs from this one.
		supervisor := filepath.Join(userHome, ".local", "libexec", "codex-app-server-supervisor")
		if cur, err := os.ReadFile(supervisor); err != nil || string(cur) != supervisorScript {
			if err := writeAtomic(supervisor, []byte(supervisorScript), 0o755); err != nil {
				return nil, nil, err
			}
			changed = append(changed, supervisor)
		}
		if err := os.MkdirAll(filepath.Join(home, "app-server-daemon"), 0o700); err != nil {
			return changed, nil, err
		}
		if err := writeAtomic(path, renderAppServerUnit(home, userHome, supervisor, codex, currentUsername()), 0o644); err != nil {
			return changed, nil, err
		}
		if err := startAppServer(r, unit); err != nil {
			return changed, nil, err
		}
		return append(changed, "app-server service "+label+" created and started"), appServerStarted(home, label, "systemd.stderr.log", r), nil
	}

	problems := verifyAppServerUnit(string(existing), home, userHome)
	for _, pr := range problems {
		notes = append(notes, fmt.Sprintf("app-server unit %s: %s (left unchanged)", path, pr))
	}
	// is-active exits non-zero for anything but active.
	if _, activeErr := systemctl(r, "is-active", unit); activeErr == nil {
		if len(problems) > 0 {
			return nil, append(notes, "app-server "+label+" is running, but its unit differs (see above)"), nil
		}
		return nil, append(notes, "app-server "+label+" verified and running"), nil
	}
	if len(problems) > 0 {
		return nil, append(notes, "app-server "+label+" was not started; fix or remove the unit above and run ./install.sh again"), nil
	}
	yes, err := p.Confirm("The Codex app-server for "+home+" is not running. Enable and start it?", true)
	if err != nil {
		return nil, notes, err
	}
	if !yes {
		return nil, append(notes, "app-server "+label+" is not running; Slack messages won't reach this Codex home until it is"), nil
	}
	if err := startAppServer(r, unit); err != nil {
		return nil, notes, err
	}
	return []string{"app-server " + label + " started"}, append(notes, appServerStarted(home, label, "systemd.stderr.log", r)...), nil
}

// startAppServer makes systemd see the unit file, then enables and starts it.
func startAppServer(r Runner, unit string) error {
	if out, err := systemctl(r, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload failed: %v: %s", err, strings.TrimSpace(out))
	}
	if out, err := systemctl(r, "enable", "--now", unit); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s failed: %v: %s", unit, err, strings.TrimSpace(out))
	}
	return nil
}
