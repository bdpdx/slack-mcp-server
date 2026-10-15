package setup

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Each home gets a start script beside the linked binary, named after the
// home's directory without its leading dot (~/.codex-test →
// start-codex-test, ~/.claude-b → start-claude-b). It runs the home's agent
// against a project directory: Codex through the home's app-server, Claude
// Code with CLAUDE_CONFIG_DIR set to the home.

const codexStartTemplate = `#!/bin/bash
# Start Codex for the agent home HOME_VALUE (written by slack-mcp-server setup).
#
# Usage: SCRIPT_NAME [-m|--model <model>] [-p|--project-root <dir>] [codex arguments...]
#   -m daybreak is short for gpt-daybreak-blue-latest; without -m, Codex uses its default model.
#   -p overrides PROJECT_ROOT below; with neither, the current directory is used.
#   Everything else is passed to codex ("--" ends these options).
#
# Set PROJECT_ROOT below to give this agent a default project directory. Re-running
# ./install.sh keeps the value you set here.
set -euo pipefail

export CODEX_HOME=HOME_QUOTED
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

const claudeStartTemplate = `#!/bin/bash
# Start Claude Code for the agent home HOME_VALUE (written by slack-mcp-server setup).
#
# Usage: SCRIPT_NAME [-p|--project-root <dir>] [claude arguments...]
#   -p overrides PROJECT_ROOT below; with neither, the current directory is used.
#   Everything else is passed to claude ("--" ends these options, so
#   SCRIPT_NAME -- -p "prompt" passes claude its own -p).
#
# Set PROJECT_ROOT below to give this agent a default project directory. Re-running
# ./install.sh keeps the value you set here.
set -euo pipefail

CLAUDE_CONFIG_LINE
export PROJECT_ROOT=PROJECT_ROOT_VALUE

me="$(basename "$0")"
args=()
while [ $# -gt 0 ]; do
	case "$1" in
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
exec claude ${args[@]+"${args[@]}"}
`

var projectRootLine = regexp.MustCompile(`(?m)^export PROJECT_ROOT=(.*)$`)

// startScriptName is the start script's file name for a home.
func startScriptName(home string) string {
	return "start-" + strings.TrimPrefix(filepath.Base(home), ".")
}

// renderStartScript writes the script for a home of type typ. projectRoot
// is the raw shell text after `export PROJECT_ROOT=`; an empty quoted string
// means none. Claude Code's default home (~/.claude, under userHome) runs
// without CLAUDE_CONFIG_DIR, as setup registers its MCP server: set to
// ~/.claude, Claude Code would read ~/.claude/.claude.json instead of
// ~/.claude.json.
func renderStartScript(typ, home, userHome, projectRoot string) []byte {
	tmpl := codexStartTemplate
	if typ == TypeClaude {
		tmpl = claudeStartTemplate
	}
	configLine := "export CLAUDE_CONFIG_DIR=HOME_QUOTED"
	if filepath.Clean(home) == filepath.Join(userHome, ".claude") {
		configLine = "unset CLAUDE_CONFIG_DIR # the default home: Claude Code reads ~/.claude and ~/.claude.json"
	}
	tmpl = strings.Replace(tmpl, "CLAUDE_CONFIG_LINE", configLine, 1)
	s := strings.NewReplacer(
		"HOME_VALUE", home,
		"SCRIPT_NAME", startScriptName(home),
		"HOME_QUOTED", singleQuote(home),
		"PROJECT_ROOT_VALUE", projectRoot,
	).Replace(tmpl)
	return []byte(s)
}

// singleQuote single-quotes s for the shell, always.
func singleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// startScriptPath is where the home's start script goes: next to bin.
func startScriptPath(bin, home string) string {
	return filepath.Join(filepath.Dir(bin), startScriptName(home))
}

// currentProjectRoot returns the raw PROJECT_ROOT text of the script at
// path, and whether the script exists.
func currentProjectRoot(path string) (string, bool) {
	old, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	if m := projectRootLine.FindSubmatch(old); m != nil {
		return string(m[1]), true
	}
	return "", true
}

// displayProjectRoot shows raw PROJECT_ROOT text as the user would write
// it: unquoted when it is simply quoted, "" when it is unset.
func displayProjectRoot(raw string) string {
	switch {
	case raw == "''" || raw == `""`:
		return ""
	case len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' && !strings.Contains(raw[1:len(raw)-1], "'"):
		return raw[1 : len(raw)-1]
	}
	return raw
}

// installStartScript writes the home's start script next to bin. root is
// the raw PROJECT_ROOT text to set; nil keeps the one in an existing copy
// (none for a new script). It returns the script's path and whether it
// changed.
func installStartScript(typ, home, userHome, bin string, root *string, now time.Time) (string, bool, error) {
	path := startScriptPath(bin, home)
	projectRoot := "''"
	if root != nil {
		projectRoot = *root
	} else if raw, _ := currentProjectRoot(path); raw != "" {
		projectRoot = raw
	}
	changed, err := replaceFile(path, renderStartScript(typ, home, userHome, projectRoot), 0o755, now)
	return path, changed, err
}
