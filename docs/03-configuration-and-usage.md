## 3. Configuration and Usage

You configure the MCP server with command line arguments and an env file passed with `--env-file` (see [Installation](02-installation.md#env-file)). The env file is the only source of `SLACK_MCP_*` settings; inherited environment variables are ignored.

### Using stdio (default)

Register the locally built binary with your MCP client:

```bash
claude mcp add -s user slack -- ~/.bin/slack-mcp-server --transport stdio --env-file ~/.claude/slack-mcp-server.env
codex mcp add slack -- ~/.bin/slack-mcp-server --transport stdio --env-file ~/.codex/slack-mcp-server.env
```

For clients configured with JSON (e.g. Claude Desktop's `claude_desktop_config.json`), use the absolute path to the binary:

```json
{
  "mcpServers": {
    "slack": {
      "command": "/Users/you/.bin/slack-mcp-server",
      "args": ["--transport", "stdio", "--env-file", "/Users/you/.claude/slack-mcp-server.env"]
    }
  }
}
```

### Using SSE or HTTP transport

Start the server with `--transport sse` (endpoint `/sse`) or `--transport http`. It listens on `127.0.0.1:13080` by default (`SLACK_MCP_HOST`, `SLACK_MCP_PORT`). Set `SLACK_MCP_API_KEY` in the env file to require `Authorization: Bearer <key>`; binding to a non-loopback address requires it.

```bash
~/.bin/slack-mcp-server --transport sse --env-file ~/.claude/slack-mcp-server.env
```

Point an SSE-capable client at `http://127.0.0.1:13080/sse` with the bearer header. Prefer keeping the server on loopback; if it must be reachable from elsewhere, put it behind a TLS-terminating reverse proxy or tunnel you control and always set `SLACK_MCP_API_KEY`.

### Using Docker

There are no published images; `docker-compose.yml` builds the image from the local `Dockerfile` (non-root, digest-pinned base images) and publishes the port on `127.0.0.1` only.

```bash
cp .env.dist slack-mcp-server.env
chmod 600 slack-mcp-server.env
# add your tokens, and set SLACK_MCP_HOST="0.0.0.0" and SLACK_MCP_API_KEY
docker network create app-tier
DOCKER_UID=$(id -u) DOCKER_GID=$(id -g) docker compose up -d --build
```

The env file is bind-mounted read-only and passed with `--env-file`; the container runs as your uid so the server accepts the file's ownership. The server must listen on `0.0.0.0` inside the container for the published port to reach it, which is why `SLACK_MCP_API_KEY` is required. `docker-compose.dev.yml` runs the server under delve, with the debugger port also bound to `127.0.0.1`.

### Console Arguments

| Argument                    | Required ? | Description                                                                                                                                                                                                         |
|-----------------------------|------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--transport` or `-t`       | Yes        | Select transport for the MCP Server, possible values are: `stdio`, `sse`                                                                                                                                            |
| `--enabled-tools` or `-e`   | No         | Comma-separated list of tools to register. If not set, all tools are registered. Runtime permissions (e.g., `SLACK_MCP_ADD_MESSAGE_TOOL`) are still enforced. Available tools: `conversations_history`, `conversations_replies`, `conversations_add_message`, `files_upload`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_search_messages`, `conversations_join`, `conversations_leave`, `conversations_unreads`, `conversations_mark`, `channels_list`, `channels_me`, `usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `users_search`. |

### Environment Variables

| Variable                          | Required? | Default                   | Description                                                                                                                                                                                                                                                                               |
|-----------------------------------|-----------|---------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SLACK_MCP_XOXP_TOKEN`            | Yes*      | `nil`                     | User OAuth token (`xoxp-...`) |
| `SLACK_MCP_XOXB_TOKEN`            | Yes*      | `nil`                     | Bot token (`xoxb-...`). If both are set, the bot token is the default and the user token is used for tools that must act as the user |
| `SLACK_MCP_PORT`                  | No        | `13080`                   | Port for the MCP server to listen on                                                                                                                                                                                                                                                      |
| `SLACK_MCP_HOST`                  | No        | `127.0.0.1`               | Host for the MCP server to listen on                                                                                                                                                                                                                                                      |
| `SLACK_MCP_API_KEY`           | No        | `nil`                     | Bearer token for SSE and HTTP transports                                                                                                                                                                                                                                                            |
| `SLACK_MCP_PROXY`                 | No        | `nil`                     | Proxy URL for outgoing requests                                                                                                                                                                                                                                                           |
| `SLACK_MCP_SERVER_CA`             | No        | `nil`                     | Path to a PEM file of extra CA certificates, added to the system trust store |
| `SLACK_MCP_ADD_MESSAGE_TOOL`      | No        | `nil`                     | Enable message posting via `conversations_add_message` by setting it to `true` for all channels, a comma-separated list of channel IDs to whitelist specific channels, or use `!` before a channel ID to allow all except specified ones. If empty, the tool is only registered when explicitly listed in `SLACK_MCP_ENABLED_TOOLS`. |
| `SLACK_MCP_UPLOAD_FILE_TOOL`      | No        | `nil`                     | Enable `files_upload` by setting it to `true` for all conversations or a comma-separated list of channel/DM IDs to allow. The tool is disabled by default. If empty, it is only registered when explicitly listed in `SLACK_MCP_ENABLED_TOOLS`. |
| `SLACK_MCP_ADD_MESSAGE_MARK`      | No        | `nil`                     | When `conversations_add_message` is enabled (via `SLACK_MCP_ADD_MESSAGE_TOOL` or `SLACK_MCP_ENABLED_TOOLS`), setting this to `true` will automatically mark sent messages as read.                                                                                                        |
| `SLACK_MCP_ADD_MESSAGE_UNFURLING` | No        | `nil`                     | Enable to let Slack unfurl posted links or set comma-separated list of domains e.g. `github.com,slack.com` to whitelist unfurling only for them. If text contains whitelisted and unknown domain unfurling will be disabled for security reasons.                                         |
| `SLACK_MCP_USERS_CACHE`           | No        | user cache dir            | Path to the users cache file. Defaults to `<user cache dir>/slack-mcp-server/<team ID>_users_cache.json` (directory mode `0700`). |
| `SLACK_MCP_CHANNELS_CACHE`        | No        | user cache dir            | Path to the channels cache file. Defaults to `<user cache dir>/slack-mcp-server/<team ID>_channels_cache_v2.json`. |
| `SLACK_MCP_CACHE_TTL`             | No        | `24h`                     | Cache time-to-live. Supports duration format (`24h`, `30m`) or seconds (`3600`). Set to `0` to disable TTL (cache forever). When the cache expires, stale data is served immediately while a background refresh fetches fresh data.                                                       |
| `SLACK_MCP_MIN_REFRESH_INTERVAL`  | No        | `30s`                     | Minimum interval between forced cache refreshes. Prevents API abuse from repeated force-refresh requests. Supports duration format (`30s`, `1m`) or seconds (`60`). Set to `0` to disable rate limiting.                                                                                  |
| `SLACK_MCP_LOG_LEVEL`             | No        | `info`                    | Log-level for stdout or stderr. Valid values are: `debug`, `info`, `warn`, `error`, `panic` and `fatal`                                                                                                                                                                                   |
| `SLACK_MCP_ENABLED_TOOLS`         | No        | `nil`                     | Comma-separated list of tools to register. If empty, all read-only tools and usergroups tools are registered; write tools (`conversations_add_message`, `files_upload`, `reactions_add`, `reactions_remove`, `attachment_get_data`) require their specific env var to be set OR must be explicitly listed here. When `files_upload` is listed here, it is enabled without channel restrictions. Available tools: `conversations_history`, `conversations_replies`, `conversations_add_message`, `files_upload`, `conversations_delete_message`, `conversations_open`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_search_messages`, `conversations_join`, `conversations_leave`, `conversations_unreads`, `conversations_mark`, `conversations_rename`, `conversations_create`, `conversations_invite`, `conversations_invite_shared`, `channels_list`, `channels_me`, `usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `users_search`. |

*Set `SLACK_MCP_XOXP_TOKEN`, `SLACK_MCP_XOXB_TOKEN`, or both. Browser session tokens (`xoxc`/`xoxd`) are not supported.

### Tool Registration and Permissions

#### Overview

Tools are controlled at two levels:
- **Registration** (`SLACK_MCP_ENABLED_TOOLS`) — determines which tools are visible to MCP clients
- **Runtime permissions** (tool-specific env vars like `SLACK_MCP_ADD_MESSAGE_TOOL`) — channel restrictions for write tools

Write tools (`conversations_add_message`, `files_upload`, `reactions_add`, `reactions_remove`, `attachment_get_data`) are **not registered by default** to prevent accidental exposure. To enable them, you must either:
1. Set their specific environment variable (e.g., `SLACK_MCP_ADD_MESSAGE_TOOL`), or
2. Explicitly list them in `SLACK_MCP_ENABLED_TOOLS`

Usergroups tools (`usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`) are **registered by default**. They require appropriate OAuth scopes (`usergroups:read` for read operations, `usergroups:write` for write operations).

`files_upload` requires the `files:write` OAuth scope. It accepts one conversation ID, a filename, and either UTF-8 text or base64 file bytes; files are limited to 5 MB. Its channel allowlist is checked after resolving channel names.


#### Examples

**Example 1: Read-only mode (default)**

By default, only read-only tools are available. No write tools are registered.

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-..."
  }
}
```

**Example 2: Enable messaging to specific channels**

Use `SLACK_MCP_ADD_MESSAGE_TOOL` to enable messaging with channel restrictions:

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-...",
    "SLACK_MCP_ADD_MESSAGE_TOOL": "C123456789,C987654321"
  }
}
```

**Example 3: Enable messaging without channel restrictions**

Use `SLACK_MCP_ENABLED_TOOLS` to register write tools without restrictions:

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-...",
    "SLACK_MCP_ENABLED_TOOLS": "conversations_history,conversations_add_message,reactions_add"
  }
}
```

**Example 4: Minimal read-only setup**

Expose only specific tools:

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-...",
    "SLACK_MCP_ENABLED_TOOLS": "channels_list,conversations_history"
  }
}
```

#### Behavior Matrix

| `ENABLED_TOOLS` | Tool-specific env var | Write tool registered? | Channel restrictions |
|-----------------|----------------------|------------------------|---------------------|
| empty/not set   | not set              | No                     | N/A                 |
| empty/not set   | `true`               | Yes                    | None                |
| empty/not set   | `C123,C456`          | Yes                    | Only listed channels |
| includes tool   | not set              | Yes                    | None                |
| includes tool   | `C123,C456`          | Yes                    | Only listed channels |
| excludes tool   | any                  | No                     | N/A                 |
