package setup

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// hostOS picks the service manager and clipboard: launchd on darwin, systemd
// otherwise. A variable so tests can run either backend on any machine.
var hostOS = runtime.GOOS

// appServerLabel names a Codex home's app-server service:
// com.openai.<dir>.app-server, with a leading "." of the directory name dropped.
func appServerLabel(home string) string {
	return "com.openai." + strings.TrimPrefix(filepath.Base(filepath.Clean(home)), ".") + ".app-server"
}

// ensureAppServer sets up (or verifies) the service that runs the Codex
// app-server for one home, with this machine's service manager.
func ensureAppServer(home, userHome string, r Runner, p Prompter) (changed, notes []string, err error) {
	repaired, err := repairDaemonPackage(home, r)
	if err != nil {
		return nil, nil, err
	}
	if hostOS == "darwin" {
		changed, notes, err = ensureAppServerLaunchd(home, userHome, r, p)
	} else {
		changed, notes, err = ensureAppServerSystemd(home, userHome, r, p)
	}
	return append(repaired, changed...), notes, err
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// supervisorScript keeps one Codex home's app-server daemon running under
// launchd or systemd. The codex binary comes from the service as $1, so the script is the
// same for every home; without it, codex on PATH.
const supervisorScript = `#!/bin/bash
set -u

# The service passes the codex binary as $1; without it, use codex on PATH.
CODEX_BIN="${1:-$(command -v codex)}"
CHECK_INTERVAL_SECONDS=15

if [ -z "$CODEX_BIN" ] || [ ! -x "$CODEX_BIN" ]; then
  echo "codex-app-server-supervisor: codex not found (${CODEX_BIN:-not on PATH})" >&2
  exit 1
fi

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
`
