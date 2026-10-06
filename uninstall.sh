#!/bin/bash
# Remove slack-mcp-server agent chat from this Mac's agent homes: builds the
# binary if needed, then runs the interactive `slack-mcp-server uninstall`.
set -euo pipefail

repo="$(cd "$(dirname "$0")" && pwd)"
cd "$repo"

die() { printf 'uninstall: %s\n' "$*" >&2; exit 1; }

if [ ! -x build/slack-mcp-server ]; then
	if ! command -v go >/dev/null 2>&1; then
		die "build/slack-mcp-server is missing and Go is not installed; install Go (https://go.dev/dl/) or run ./install.sh first."
	fi
	make build
fi

exec build/slack-mcp-server uninstall --repo "$repo"
