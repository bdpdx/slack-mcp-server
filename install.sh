#!/bin/bash
# Set up slack-mcp-server agent chat on this Mac or Debian machine: prerequisites,
# build, install, then the interactive `slack-mcp-server setup`. Safe to re-run.
set -euo pipefail

start_dir="$PWD"
repo="$(cd "$(dirname "$0")" && pwd)"
cd "$repo"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

need_go="$(awk '/^go /{print $2; exit}' go.mod)"
version_ok() { # $1 have, $2 need (x.y[.z]); true if have >= need
	[ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -n1)" = "$2" ]
}
go_dir="$HOME/.local/go"

# install_go puts the latest Go release in $go_dir, checking its SHA-256.
install_go() {
	local arch version file tmp sum
	case "$(uname -m)" in
		x86_64) arch=amd64 ;;
		aarch64) arch=arm64 ;;
		*) die "no Go release for $(uname -m) here; install Go $need_go or newer (https://go.dev/dl/), then run ./install.sh again." ;;
	esac
	if [ -e "$go_dir" ] && [ ! -f "$go_dir/VERSION" ]; then
		die "$go_dir exists and is not a Go release; move it, then run ./install.sh again."
	fi
	version="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -n1)"
	[ -n "$version" ] || die "could not find the latest Go version."
	file="$version.linux-$arch.tar.gz"
	mkdir -p "$(dirname "$go_dir")"
	tmp="$(mktemp -d "$(dirname "$go_dir")/.go-install.XXXXXX")"
	trap 'rm -rf "$tmp"' EXIT
	say "Downloading $file..."
	curl -fsSL "https://dl.google.com/go/$file" -o "$tmp/$file" || die "could not download $file."
	sum="$(curl -fsSL "https://dl.google.com/go/$file.sha256")" || die "could not download the checksum of $file."
	printf '%s  %s\n' "$sum" "$tmp/$file" | sha256sum -c --quiet - || die "$file failed its checksum."
	tar -xzf "$tmp/$file" -C "$tmp"
	rm -rf "$go_dir"
	mv "$tmp/go" "$go_dir"
	rm -rf "$tmp"
	trap - EXIT
	export PATH="$go_dir/bin:$PATH"
	hash -r
}
prereqs_darwin() {
	# Command Line Tools (git, make, and Homebrew need them), then Go from Homebrew.
	if ! xcode-select -p >/dev/null 2>&1; then
		say "Apple's Command Line Tools are required. Starting their installer..."
		xcode-select --install || true
		die "run ./install.sh again when the Command Line Tools installation finishes."
	fi

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
}

prereqs_linux() {
	# Packages (git, make, and curl and tar to fetch Go).
	missing=""
	for tool in git make curl tar; do
		command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
	done
	if [ -n "$missing" ]; then
		command -v apt-get >/dev/null 2>&1 || die "install these, then run ./install.sh again:$missing"
		sudo=""
		if [ "$(id -u)" != 0 ]; then
			command -v sudo >/dev/null 2>&1 || die "as root, run: apt-get install$missing ca-certificates, then run ./install.sh again."
			sudo="sudo"
		fi
		say "Required packages are missing:$missing"
		printf 'Install them with apt-get now? [Y/n] '
		read -r answer
		case "$answer" in
			[nN]*) die "install them, then run ./install.sh again." ;;
		esac
		$sudo apt-get update
		# shellcheck disable=SC2086 # one word per package
		$sudo apt-get install -y $missing ca-certificates
	fi

	# Go. Debian's package is too old, so
	# this uses the official release, kept in ~/.local/go.
	[ ! -x "$go_dir/bin/go" ] || export PATH="$go_dir/bin:$PATH"
	have_go=""
	if command -v go >/dev/null 2>&1; then
		have_go="$(go env GOVERSION 2>/dev/null | sed 's/^go//' || true)"
		[ -n "$have_go" ] || have_go="$(go version | awk '{print $3}' | sed 's/^go//')"
	fi
	if [ -z "$have_go" ] || ! version_ok "$have_go" "$need_go"; then
		say "Go $need_go or newer is required (found: ${have_go:-none})."
		printf 'Install the latest release to %s now? [Y/n] ' "$go_dir"
		read -r answer
		case "$answer" in
			[nN]*) die "install Go $need_go or newer (https://go.dev/dl/), then run ./install.sh again." ;;
		esac
		install_go
		have_go="$(go env GOVERSION | sed 's/^go//')"
		version_ok "$have_go" "$need_go" || die "the latest Go is $have_go, older than the $need_go go.mod asks for."
	fi
}

# 1 and 2. Build tools, and Go at least as new as go.mod asks for.
case "$(uname -s)" in
	Darwin) prereqs_darwin ;;
	Linux) prereqs_linux ;;
	*) die "this installer supports macOS and Debian Linux only." ;;
esac

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

# slackmcpctl (app-server services, slack-mcp-server processes) goes next to it.
ctl="$(dirname "$bin")/slackmcpctl"
if [ -d "$ctl" ] || { [ -e "$ctl" ] && [ ! -L "$ctl" ] && ! sed -n 2p "$ctl" | grep -q '^# slackmcpctl:'; }; then
	say "$ctl exists and is not slackmcpctl; not installing slackmcpctl there"
else
	install_copy "$repo/scripts/slackmcpctl" "$ctl" || die "could not install $ctl"
	say "Installed $ctl"
fi

# 5. Interactive setup.
exec "$bin" setup --repo "$repo" --bin "$bin"
