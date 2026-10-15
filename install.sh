#!/bin/bash
# Set up slack-mcp-server agent chat on this Mac: prerequisites, build,
# install, then the interactive `slack-mcp-server setup`. Safe to re-run.
set -euo pipefail

start_dir="$PWD"
repo="$(cd "$(dirname "$0")" && pwd)"
cd "$repo"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

if [ "$(uname -s)" != "Darwin" ]; then
	die "this installer supports macOS only."
fi

# 1. Command Line Tools (git, make, and Homebrew need them).
if ! xcode-select -p >/dev/null 2>&1; then
	say "Apple's Command Line Tools are required. Starting their installer..."
	xcode-select --install || true
	die "run ./install.sh again when the Command Line Tools installation finishes."
fi

# 2. Go, at least the version go.mod asks for.
need_go="$(awk '/^go /{print $2; exit}' go.mod)"
version_ok() { # $1 have, $2 need (x.y[.z]); true if have >= need
	[ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -n1)" = "$2" ]
}
have_go=""
if command -v go >/dev/null 2>&1; then
	have_go="$(go env GOVERSION | sed 's/^go//')"
fi
if [ -z "$have_go" ] || ! version_ok "$have_go" "$need_go"; then
	if command -v brew >/dev/null 2>&1; then
		say "Go $need_go or newer is required (found: ${have_go:-none})."
		printf 'Install it with Homebrew now? [Y/n] '
		read -r answer
		case "$answer" in
			[nN]*) die "install Go $need_go or newer (https://go.dev/dl/), then run ./install.sh again." ;;
		esac
		if brew list go >/dev/null 2>&1; then brew upgrade go; else brew install go; fi
		hash -r
		have_go="$(go env GOVERSION 2>/dev/null | sed 's/^go//' || true)"
		if [ -z "$have_go" ] || ! version_ok "$have_go" "$need_go"; then
			say "Go $need_go or newer is still not the first go on your PATH."
			say "The go in use is $(command -v go || echo 'not found') (version: ${have_go:-unknown})."
			die "remove or update that Go, or put Homebrew's bin directory ($(brew --prefix)/bin) first on your PATH, then run ./install.sh again."
		fi
	else
		say "Go $need_go or newer is required (found: ${have_go:-none}), and Homebrew isn't installed."
		say "Install Homebrew (https://brew.sh) and run ./install.sh again,"
		say "or install Go directly from https://go.dev/dl/."
		exit 1
	fi
fi

# 3. Build.
make build

# 4. Install the binary.
# install_copy SRC DEST: copy SRC to DEST (mode 0755) through a temporary file
# and a rename, so DEST is never half-written and processes still running the
# old file keep it. A symlink at DEST (from an older install) is replaced.
install_copy() {
	local tmp
	tmp="$(mktemp "$(dirname "$2")/.$(basename "$2").XXXXXX")" || return 1
	if cp "$1" "$tmp" && chmod 0755 "$tmp" && mv -f "$tmp" "$2"; then
		return 0
	fi
	rm -f "$tmp"
	return 1
}

state="$repo/.install-state.json"
default_bin=""
if [ -f "$state" ]; then
	default_bin="$(sed -n 's/.*"bin": *"\([^"]*\)".*/\1/p' "$state" | head -n1)"
fi
if [ -z "$default_bin" ]; then
	default_bin="$(./build/slack-mcp-server setup --repo "$repo" --print-existing-bin 2>/dev/null || true)"
fi
if [ -z "$default_bin" ]; then
	default_bin="$HOME/.local/bin/slack-mcp-server"
fi
while true; do
	printf '\nWhere should the slack-mcp-server binary be installed?\nPress Enter for %s: ' "$default_bin"
	read -r bin
	case "$bin" in
		"") bin="$default_bin" ;;
		*/* | "~"*) ;;
		*)
			say "Enter a path such as ~/.bin/slack-mcp-server, or press Enter to use the default."
			continue
			;;
	esac
	case "$bin" in
		"~"/*) bin="$HOME/${bin#\~/}" ;;
	esac
	case "$bin" in
		/*) ;;
		*) bin="$start_dir/$bin" ;; # relative to where install.sh was run
	esac
	# Only an earlier install (a copy, or a symlink from before copies) is replaced.
	if [ -d "$bin" ] || { [ -e "$bin" ] && [ ! -L "$bin" ] && ! grep -qa "github.com/bdpdx/slack-mcp-server" "$bin"; }; then
		say "$bin exists and is not a slack-mcp-server binary; choose another path or move it"
		continue
	fi
	break
done
mkdir -p "$(dirname "$bin")"
install_copy "$repo/build/slack-mcp-server" "$bin" || die "could not install $bin"
say "Installed $bin"

# slackmcpctl (app-server launch agents, slack-mcp-server processes) goes next to it.
ctl="$(dirname "$bin")/slackmcpctl"
if [ -d "$ctl" ] || { [ -e "$ctl" ] && [ ! -L "$ctl" ] && ! sed -n 2p "$ctl" | grep -q '^# slackmcpctl:'; }; then
	say "$ctl exists and is not slackmcpctl; not installing slackmcpctl there"
else
	install_copy "$repo/scripts/slackmcpctl" "$ctl" || die "could not install $ctl"
	say "Installed $ctl"
fi

# 5. Interactive setup.
exec "$bin" setup --repo "$repo" --bin "$bin"
