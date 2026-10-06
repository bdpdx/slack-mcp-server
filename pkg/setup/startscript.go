package setup

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Each Codex home gets a start script beside the linked binary, named after
// the home's directory without its leading dot (~/.codex-test →
// start-codex-test). It runs Codex for that home against a project
// directory, through the home's app-server.

const startScriptTemplate = `#!/bin/bash
# Start Codex for the agent home CODEX_HOME_VALUE (written by slack-mcp-server setup).
#
# Usage: SCRIPT_NAME [-m|--model <model>] [-p|--project-root <dir>] [codex arguments...]
#   -m daybreak is short for gpt-daybreak-blue-latest; without -m, Codex uses its default model.
#   -p overrides PROJECT_ROOT below; with neither, the current directory is used.
#   Everything else is passed to codex ("--" ends these options).
#
# Set PROJECT_ROOT below to give this agent a default project directory. Re-running
# ./install.sh keeps the value you set here.
set -euo pipefail

export CODEX_HOME=CODEX_HOME_QUOTED
export PROJECT_ROOT=PROJECT_ROOT_VALUE

me="$(basename "$0")"
model=""
args=()
while [ $# -gt 0 ]; do
	case "$1" in
		-m|--model)
			if [ $# -lt 2 ]; then
				echo "$me: $1 needs a model name" >&2
				exit 2
			fi
			model="$2"
			shift 2
			;;
		-p|--project-root)
			if [ $# -lt 2 ]; then
				echo "$me: $1 needs a directory" >&2
				exit 2
			fi
			PROJECT_ROOT="$2"
			shift 2
			;;
		--)
			shift
			args+=("$@")
			break
			;;
		*)
			args+=("$1")
			shift
			;;
	esac
done

case "$model" in
	daybreak) model="gpt-daybreak-blue-latest" ;;
esac

case "$PROJECT_ROOT" in
	"~"/*) PROJECT_ROOT="$HOME/${PROJECT_ROOT#\~/}" ;;
esac
if [ -z "$PROJECT_ROOT" ]; then
	PROJECT_ROOT="$PWD"
fi
if [ ! -d "$PROJECT_ROOT" ]; then
	echo "$me: project root $PROJECT_ROOT is not a directory" >&2
	exit 1
fi
export PROJECT_ROOT

cd "$PROJECT_ROOT"
if [ -n "$model" ]; then
	exec codex --remote unix:// -C "$PROJECT_ROOT" -m "$model" ${args[@]+"${args[@]}"}
fi
exec codex --remote unix:// -C "$PROJECT_ROOT" ${args[@]+"${args[@]}"}
`

var projectRootLine = regexp.MustCompile(`(?m)^export PROJECT_ROOT=(.*)$`)

// startScriptName is the start script's file name for a Codex home.
func startScriptName(home string) string {
	return "start-" + strings.TrimPrefix(filepath.Base(home), ".")
}

// renderStartScript writes the script for home. projectRoot is the raw
// shell text after `export PROJECT_ROOT=` ('' for none).
func renderStartScript(home, projectRoot string) []byte {
	s := strings.NewReplacer(
		"CODEX_HOME_VALUE", home,
		"SCRIPT_NAME", startScriptName(home),
		"CODEX_HOME_QUOTED", "'"+strings.ReplaceAll(home, "'", `'\''`)+"'",
		"PROJECT_ROOT_VALUE", projectRoot,
	).Replace(startScriptTemplate)
	return []byte(s)
}

// installStartScript writes the home's start script next to bin, keeping a
// PROJECT_ROOT the user set in an existing copy. It returns the script's
// path and whether it changed.
func installStartScript(home, bin string, now time.Time) (string, bool, error) {
	path := filepath.Join(filepath.Dir(bin), startScriptName(home))
	projectRoot := "''"
	if old, err := os.ReadFile(path); err == nil {
		if m := projectRootLine.FindSubmatch(old); m != nil {
			projectRoot = string(m[1])
		}
	}
	changed, err := replaceFile(path, renderStartScript(home, projectRoot), 0o755, now)
	return path, changed, err
}
