### 2. Installation

This fork is built from source only. Do not use the upstream npm package (`slack-mcp-server`), container images, DXT extension or release binaries; they are built and published by a different party.

#### Build

Requires Go (see `go.mod` for the version) and `make`.

```bash
git clone https://github.com/bdpdx/slack-mcp-server.git
cd slack-mcp-server
make build
mkdir -p ~/.bin && ln -s "$PWD/build/slack-mcp-server" ~/.bin/
```

`make build` formats the code and builds `build/slack-mcp-server` with `-mod=readonly`, so it never rewrites `go.mod`/`go.sum`; run `make tidy` separately when you change dependencies. Because `~/.bin/slack-mcp-server` is a symlink, later `make build` runs update it in place. Run `make help` for the other targets.

#### Env file

The server reads its settings only from the file passed with `--env-file` (inherited `SLACK_MCP_*` environment variables are ignored). The file may set only `SLACK_MCP_*` variables, must belong to you, and must not be readable by others:

```bash
cp .env.dist ~/.claude/slack-mcp-server.env
chmod 600 ~/.claude/slack-mcp-server.env
# then add your tokens (see Authentication Setup)
```

Use one file per client home, e.g. `~/.claude/slack-mcp-server.env` and `~/.codex/slack-mcp-server.env`. Without `--env-file`, the server uses the file in the detected Claude Code or Codex home and exits if it is missing.

#### Register with your MCP client

```bash
claude mcp add -s user slack -- ~/.bin/slack-mcp-server --transport stdio --env-file ~/.claude/slack-mcp-server.env
codex mcp add slack -- ~/.bin/slack-mcp-server --transport stdio --env-file ~/.codex/slack-mcp-server.env
```

For other stdio clients (e.g. Claude Desktop or Cursor), configure the command `~/.bin/slack-mcp-server` (use the absolute path) with the arguments `--transport stdio --env-file <path to env file>`.

See [Slack Agent Chat](04-slack-agent-chat.md) for the agent-chat skills and hooks (`make install-agent-chat`), and [Configuration and Usage](03-configuration-and-usage.md) for SSE/HTTP transports and Docker.

See next: [Configuration and Usage](03-configuration-and-usage.md)
