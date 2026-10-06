package setup

import (
	"fmt"
	"html"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// appServerLabel is the launchd label for a Codex home's app-server agent:
// com.openai.<dir>.app-server, with a leading "." of the directory name dropped.
func appServerLabel(home string) string {
	return "com.openai." + strings.TrimPrefix(filepath.Base(filepath.Clean(home)), ".") + ".app-server"
}

// renderAppServerPlist renders the launchd plist that runs the supervisor
// for one Codex home.
func renderAppServerPlist(home, userHome, supervisor, username string) []byte {
	path := userHome + "/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	logs := filepath.Join(home, "app-server-daemon")
	s := func(v string) string { return "<string>" + html.EscapeString(v) + "</string>" }
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>EnvironmentVariables</key>
	<dict>
`)
	for _, kv := range [][2]string{{"CODEX_HOME", home}, {"HOME", userHome}, {"LOGNAME", username}, {"PATH", path}, {"USER", username}} {
		b.WriteString("\t\t<key>" + kv[0] + "</key>\n\t\t" + s(kv[1]) + "\n")
	}
	b.WriteString("\t</dict>\n\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	b.WriteString("\t<key>Label</key>\n\t" + s(appServerLabel(home)) + "\n")
	b.WriteString("\t<key>ProcessType</key>\n\t<string>Background</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n\t\t" + s(supervisor) + "\n\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t" + s(filepath.Join(logs, "launchagent.stderr.log")) + "\n")
	b.WriteString("\t<key>StandardOutPath</key>\n\t" + s(filepath.Join(logs, "launchagent.stdout.log")) + "\n")
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n</dict>\n</plist>\n")
	return []byte(b.String())
}

func supervisorScript(codexBin string) []byte {
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(codexBin)
	return []byte(`#!/bin/bash
set -u

CODEX_BIN="` + q + `"
CHECK_INTERVAL_SECONDS=15

stop_daemon() {
  trap - TERM INT HUP
  "$CODEX_BIN" app-server daemon stop >/dev/null 2>&1 || true
  exit 0
}

trap stop_daemon TERM INT HUP

if ! "$CODEX_BIN" app-server daemon start; then
  exit 1
fi

while "$CODEX_BIN" app-server daemon version >/dev/null 2>&1; do
  sleep "$CHECK_INTERVAL_SECONDS" &
  wait $!
done

exit 1
`)
}

var (
	plistLabelRe   = regexp.MustCompile(`<key>Label</key>\s*<string>(.*?)</string>`)
	plistHomeRe    = regexp.MustCompile(`<key>CODEX_HOME</key>\s*<string>(.*?)</string>`)
	plistProgramRe = regexp.MustCompile(`<key>ProgramArguments</key>\s*<array>\s*<string>(.*?)</string>`)
	stateRunningRe = regexp.MustCompile(`(?m)^\s*state = running\s*$`)
)

func plistValue(re *regexp.Regexp, text string) (string, bool) {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(m[1]), true
}

// verifyAppServerPlist returns one problem per mismatch with what this
// installer would have written.
func verifyAppServerPlist(text, home, userHome, label string) []string {
	var problems []string
	if v, ok := plistValue(plistLabelRe, text); !ok || v != label {
		problems = append(problems, fmt.Sprintf("Label is %q, expected %q", v, label))
	}
	v, ok := plistValue(plistHomeRe, text)
	switch {
	case ok && filepath.Clean(v) != filepath.Clean(home):
		problems = append(problems, fmt.Sprintf("CODEX_HOME is %q, expected %q", v, home))
	case !ok && filepath.Clean(home) != filepath.Join(userHome, ".codex"):
		problems = append(problems, fmt.Sprintf("CODEX_HOME is not set, expected %q", home))
	}
	if prog, ok := plistValue(plistProgramRe, text); !ok {
		problems = append(problems, "no program in ProgramArguments")
	} else if _, err := os.Stat(prog); err != nil {
		problems = append(problems, fmt.Sprintf("program %s does not exist", prog))
	}
	return problems
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// ensureAppServer sets up (or verifies) the launchd agent that runs the
// Codex app-server for one home. A missing plist is created and started; an
// existing one is verified and never rewritten.
func ensureAppServer(home, userHome string, r Runner, p Prompter) (changed, notes []string, err error) {
	label := appServerLabel(home)
	plist := filepath.Join(userHome, "Library", "LaunchAgents", label+".plist")
	domain := "gui/" + strconv.Itoa(os.Getuid())
	target := domain + "/" + label

	existing, readErr := os.ReadFile(plist)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, nil, readErr
	}
	if readErr != nil {
		supervisor := filepath.Join(userHome, ".local", "libexec", "codex-app-server-supervisor")
		if _, err := os.Stat(supervisor); err != nil {
			codex, err := r.LookPath("codex")
			if err != nil {
				return nil, []string{"Codex CLI not found; install Codex, then run ./install.sh again to set up its app-server"}, nil
			}
			if err := writeAtomic(supervisor, supervisorScript(codex), 0o755); err != nil {
				return nil, nil, err
			}
			changed = append(changed, supervisor)
		}
		if err := os.MkdirAll(filepath.Join(home, "app-server-daemon"), 0o700); err != nil {
			return changed, nil, err
		}
		if err := writeAtomic(plist, renderAppServerPlist(home, userHome, supervisor, currentUsername()), 0o644); err != nil {
			return changed, nil, err
		}
		if out, err := r.Run(nil, "launchctl", "enable", target); err != nil {
			return changed, nil, fmt.Errorf("launchctl enable %s failed: %v: %s", target, err, strings.TrimSpace(out))
		}
		if out, err := r.Run(nil, "launchctl", "bootstrap", domain, plist); err != nil {
			return changed, nil, fmt.Errorf("launchctl bootstrap %s failed: %v: %s", domain, err, strings.TrimSpace(out))
		}
		return append(changed, "app-server launch agent "+label+" created and started"), nil, nil
	}

	problems := verifyAppServerPlist(string(existing), home, userHome, label)
	for _, pr := range problems {
		notes = append(notes, fmt.Sprintf("app-server plist %s: %s (left unchanged)", plist, pr))
	}
	out, printErr := r.Run(nil, "launchctl", "print", target)
	if printErr == nil && stateRunningRe.MatchString(out) {
		if len(problems) > 0 {
			return nil, append(notes, "app-server "+label+" is running, but its plist differs (see above)"), nil
		}
		return nil, append(notes, "app-server "+label+" verified and running"), nil
	}
	if len(problems) > 0 {
		return nil, append(notes, "app-server "+label+" was not started; fix or remove the plist above and run ./install.sh again"), nil
	}
	yes, err := p.Confirm("The Codex app-server for "+home+" is not running. Enable and start it?", true)
	if err != nil {
		return nil, notes, err
	}
	if !yes {
		return nil, append(notes, "app-server "+label+" is not running; Slack messages won't reach this Codex home until it is"), nil
	}
	if out, err := r.Run(nil, "launchctl", "enable", target); err != nil {
		return nil, notes, fmt.Errorf("launchctl enable %s failed: %v: %s", target, err, strings.TrimSpace(out))
	}
	if printErr == nil {
		if out, err := r.Run(nil, "launchctl", "kickstart", "-k", target); err != nil {
			return nil, notes, fmt.Errorf("launchctl kickstart %s failed: %v: %s", target, err, strings.TrimSpace(out))
		}
	} else if out, err := r.Run(nil, "launchctl", "bootstrap", domain, plist); err != nil {
		return nil, notes, fmt.Errorf("launchctl bootstrap %s failed: %v: %s", domain, err, strings.TrimSpace(out))
	}
	return []string{"app-server " + label + " started"}, notes, nil
}
