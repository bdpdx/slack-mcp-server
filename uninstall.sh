#!/bin/bash
# Remove slack-mcp-server agent chat from this Mac's agent homes: builds the
# binary if needed, then runs the interactive `slack-mcp-server uninstall`.
set -euo pipefail

repo="$(cd "$(dirname "$0")" && pwd)"
cd "$repo"

die() { printf 'uninstall: %s\n' "$*" >&2; exit 1; }

# Rebuild when Go is available, so the binary knows the uninstall command
# even if it was built before this checkout was updated.
if command -v go >/dev/null 2>&1; then
	make build
elif [ ! -x build/slack-mcp-server ]; then
	die "build/slack-mcp-server is missing and Go is not installed; install Go (https://go.dev/dl/) or run ./install.sh first."
fi

exec build/slack-mcp-server uninstall --repo "$repo"
