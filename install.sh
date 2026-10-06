#!/bin/bash
# Set up slack-mcp-server agent chat on this Mac: prerequisites, build,
# link, then the interactive `slack-mcp-server setup`. Safe to re-run.
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

# 4. Link the binary.
state="$repo/.install-state.json"
default_link=""
if [ -f "$state" ]; then
	default_link="$(sed -n 's/.*"bin": *"\([^"]*\)".*/\1/p' "$state" | head -n1)"
fi
if [ -z "$default_link" ]; then
	default_link="$(./build/slack-mcp-server setup --repo "$repo" --print-existing-bin 2>/dev/null || true)"
fi
if [ -z "$default_link" ]; then
	default_link="$HOME/.local/bin/slack-mcp-server"
fi
while true; do
	printf 'Where should the slack-mcp-server binary be linked? Press Enter for %s: ' "$default_link"
	read -r link
	case "$link" in
		"") link="$default_link" ;;
		*/* | "~"*) ;;
		*)
			say "Enter a path such as ~/.bin/slack-mcp-server, or press Enter to use the default."
			continue
			;;
	esac
	case "$link" in
		"~"/*) link="$HOME/${link#\~/}" ;;
	esac
	case "$link" in
		/*) ;;
		*) link="$start_dir/$link" ;; # relative to where install.sh was run
	esac
	if [ -e "$link" ] && [ ! -L "$link" ]; then
		say "$link exists and is not a symlink; choose another path or move it"
		continue
	fi
	break
done
mkdir -p "$(dirname "$link")"
ln -sfn "$repo/build/slack-mcp-server" "$link"
say "Linked $link -> $repo/build/slack-mcp-server"

# 5. Interactive setup.
exec "$link" setup --repo "$repo" --bin "$link"
