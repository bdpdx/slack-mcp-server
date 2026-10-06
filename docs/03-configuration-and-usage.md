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
| `--enabled-tools` or `-e`   | No         | Comma-separated list of tools to register (overrides `SLACK_MCP_ENABLED_TOOLS`). If not set, all read-only tools are registered and write tools follow their own variables. Runtime permissions (e.g., `SLACK_MCP_ADD_MESSAGE_TOOL`) are still enforced. Available tools: `conversations_history`, `conversations_replies`, `conversations_add_message`, `files_upload`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_search_messages`, `conversations_join`, `conversations_leave`, `conversations_unreads`, `conversations_mark`, `channels_list`, `channels_me`, `usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `users_search`. |

### Environment Variables

| Variable                          | Required? | Default                   | Description                                                                                                                                                                                                                                                                               |
|-----------------------------------|-----------|---------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SLACK_MCP_XOXP_TOKEN`            | Yes*      | `nil`                     | User OAuth token (`xoxp-...`) |
| `SLACK_MCP_XOXB_TOKEN`            | Yes*      | `nil`                     | Bot token (`xoxb-...`). If both are set, the bot token is the default and the user token is used for tools that must act as the user |
| `SLACK_MCP_PORT`                  | No        | `13080`                   | Port for the MCP server to listen on                                                                                                                                                                                                                                                      |
| `SLACK_MCP_HOST`                  | No        | `127.0.0.1`               | Host for the MCP server to listen on. A non-loopback host (including `0.0.0.0`) always requires `SLACK_MCP_API_KEY`. |
| `SLACK_MCP_API_KEY`               | Yes for `sse`/`http` | `nil`          | Bearer token required on every request to the SSE and HTTP transports (including the SSE session `GET`). The server refuses to start these transports without it unless `SLACK_MCP_ALLOW_UNAUTHENTICATED=true` and the host is loopback. `SLACK_MCP_SSE_API_KEY` is accepted as a deprecated alias. |
| `SLACK_MCP_ALLOW_UNAUTHENTICATED` | No        | `false`                   | Boolean. Allows the `sse`/`http` transports to run without `SLACK_MCP_API_KEY`, only when `SLACK_MCP_HOST` is a loopback address. |
| `SLACK_MCP_ALLOWED_ORIGINS`       | No        | `nil`                     | Comma-separated browser origins (e.g. `https://app.example.com`) allowed to call the `sse`/`http` transports. Requests carrying any other `Origin` header are rejected; `*` is not accepted. |
| `SLACK_MCP_ALLOWED_HOSTS`         | No        | `nil`                     | Extra `Host` header values (`name` or `name:port`) accepted by the `sse`/`http` transports, e.g. the name of a reverse proxy. By default only the bind host, `localhost`, `127.0.0.1` and `[::1]` with the listening port are accepted (DNS-rebinding protection). |
| `SLACK_MCP_PROXY`                 | No        | `nil`                     | Proxy URL for outgoing requests                                                                                                                                                                                                                                                           |
| `SLACK_MCP_SERVER_CA`             | No        | `nil`                     | Path to a PEM file of extra CA certificates, added to the system trust store |
| `SLACK_MCP_ADD_MESSAGE_TOOL`      | No        | `nil`                     | Channel policy for `conversations_add_message`: `true`/`1`/`yes`/`on` for all channels, `false`/`0`/`no`/`off` or empty for off, a comma-separated allow-list of channel IDs or names (`C123,#general,@alice`), or a deny-list with every entry prefixed by `!`. Mixing allowed and denied entries is a startup error. A user ID entry (`U123`) stands for the DM with that user. If empty, the tool is only enabled when explicitly listed in `SLACK_MCP_ENABLED_TOOLS`. |
| `SLACK_MCP_UPLOAD_FILE_TOOL`      | No        | `nil`                     | Channel policy for `files_upload`, same syntax as `SLACK_MCP_ADD_MESSAGE_TOOL`. Off by default. |
| `SLACK_MCP_DELETE_MESSAGE_TOOL`   | No        | `nil`                     | Channel policy for `conversations_delete_message`, same syntax as `SLACK_MCP_ADD_MESSAGE_TOOL`. Off by default. |
| `SLACK_MCP_REACTION_TOOL`         | No        | `nil`                     | Channel policy for `reactions_add` and `reactions_remove`, same syntax as `SLACK_MCP_ADD_MESSAGE_TOOL`. Off by default. |
| `SLACK_MCP_ALLOW_AS_USER`         | No        | `false`                   | Boolean. Allows `as_user=true` on `conversations_add_message`, `reactions_add`/`reactions_remove` and `files_upload`; otherwise such calls are refused. When only a user token (`xoxp`) is configured, every action is necessarily taken as that user regardless of this setting. |
| `SLACK_MCP_ATTACHMENT_TOOL`       | No        | `false`                   | Boolean. Enables `attachment_get_data`. External files (hosted outside Slack) are refused; downloads are limited to Slack hosts. Inline results are capped at 1 MiB (images 5 MiB); with `save=true` the file (up to 64 MiB) is saved into the files folder and its path returned. |
| `SLACK_MCP_FILES_DIR`             | No        | `~/Downloads/slack-mcp`   | The only local folder files move through: `attachment_get_data save=true` writes into it and `files_upload path=` reads only from it. Created (mode 0700) at startup if missing. |
| `SLACK_MCP_OPEN_CONVERSATION_TOOL`| No        | `false`                   | Boolean. Enables `conversations_open`. |
| `SLACK_MCP_MARK_TOOL`             | No        | `false`                   | Boolean. Enables `conversations_mark` (registered only with a user token, and always acts as the user). |
| `SLACK_MCP_JOIN_TOOL`             | No        | `false`                   | Boolean. Enables `conversations_join` and `conversations_leave`. |
| `SLACK_MCP_RENAME_CHANNEL_TOOL`   | No        | `false`                   | Boolean. Enables `conversations_rename`. |
| `SLACK_MCP_CREATE_CHANNEL_TOOL`   | No        | `false`                   | Boolean. Enables `conversations_create`. |
| `SLACK_MCP_SET_TOPIC_TOOL`        | No        | `false`                   | Boolean. Enables `conversations_set_topic`. |
| `SLACK_MCP_INVITE_TOOL`           | No        | `false`                   | Boolean. Enables `conversations_invite`. |
| `SLACK_MCP_INVITE_SHARED_TOOL`    | No        | `false`                   | Boolean. Enables `conversations_invite_shared` (sends real Slack Connect invites). |
| `SLACK_MCP_USERGROUPS_WRITE_TOOL` | No        | `false`                   | Boolean. Enables `usergroups_create`, `usergroups_update`, `usergroups_users_update`, and the `join`/`leave` actions of `usergroups_me`. |
| `SLACK_MCP_ADD_MESSAGE_MARK`      | No        | `false`                   | Boolean. When `conversations_add_message` is enabled, marks sent messages as read. |
| `SLACK_MCP_ADD_MESSAGE_UNFURLING` | No        | `false`                   | `true` to let Slack unfurl every posted link, or a comma-separated domain allow-list e.g. `github.com,slack.com`. Every URL and bare domain in the text and in `blocks` must be on the list; if any is not (or cannot be parsed), unfurling is disabled for that message. |
| `SLACK_MCP_USERS_CACHE`           | No        | user cache dir            | Path to the users cache file. Defaults to `<user cache dir>/slack-mcp-server/<team ID>_users_cache.json` (directory mode `0700`). |
| `SLACK_MCP_CHANNELS_CACHE`        | No        | user cache dir            | Path to the channels cache file. Defaults to `<user cache dir>/slack-mcp-server/<team ID>_channels_cache_v2.json`. |
| `SLACK_MCP_CACHE_TTL`             | No        | `24h`                     | Cache time-to-live. Supports duration format (`24h`, `30m`) or seconds (`3600`). Set to `0` to disable TTL (cache forever). When the cache expires, stale data is served immediately while a background refresh fetches fresh data.                                                       |
| `SLACK_MCP_MIN_REFRESH_INTERVAL`  | No        | `30s`                     | Minimum interval between forced cache refreshes. Prevents API abuse from repeated force-refresh requests. Supports duration format (`30s`, `1m`) or seconds (`60`). Set to `0` to disable rate limiting.                                                                                  |
| `SLACK_MCP_LOG_LEVEL`             | No        | `info`                    | Log-level for stdout or stderr. Valid values are: `debug`, `info`, `warn`, `error`, `panic` and `fatal`                                                                                                                                                                                   |
| `SLACK_MCP_ENABLED_TOOLS`         | No        | `nil`                     | Comma-separated list of tools to register (same as `-e`). If empty, all read-only tools are registered and every tool that changes something stays off until its own variable enables it. A write tool named here is enabled (without channel restrictions) when its own variable is unset; an explicit `false` in its variable still disables it. Available tools: `conversations_history`, `conversations_replies`, `conversations_add_message`, `files_upload`, `conversations_delete_message`, `conversations_open`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_search_messages`, `conversations_join`, `conversations_leave`, `conversations_unreads`, `conversations_mark`, `conversations_rename`, `conversations_create`, `conversations_set_topic`, `conversations_invite`, `conversations_invite_shared`, `channels_list`, `channels_me`, `usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `users_search`. |

*Set `SLACK_MCP_XOXP_TOKEN`, `SLACK_MCP_XOXB_TOKEN`, or both. Browser session tokens (`xoxc`/`xoxd`) are not supported.

### Tool Registration and Permissions

#### Overview

Tools are controlled at two levels:
- **Registration** (`SLACK_MCP_ENABLED_TOOLS`) — determines which tools are visible to MCP clients
- **Runtime permissions** (tool-specific env vars like `SLACK_MCP_ADD_MESSAGE_TOOL`) — channel restrictions for write tools

Every tool that changes something is **off by default** and is neither registered nor callable until enabled. To enable one, either:
1. Set its environment variable (see the table above), or
2. Explicitly list it in `SLACK_MCP_ENABLED_TOOLS` / `-e` while leaving its variable unset.

Boolean variables accept `true`/`1`/`yes`/`on` and `false`/`0`/`no`/`off`/empty (case-insensitive); any other value is a startup error. An explicit off value always wins. Registration and the runtime checks use the same parsed configuration, so a tool that is not registered also refuses to run.

| Tools | Enabled by |
|-------|------------|
| `conversations_add_message` | `SLACK_MCP_ADD_MESSAGE_TOOL` (channel policy) |
| `conversations_delete_message` | `SLACK_MCP_DELETE_MESSAGE_TOOL` (channel policy) |
| `reactions_add`, `reactions_remove` | `SLACK_MCP_REACTION_TOOL` (channel policy) |
| `files_upload` | `SLACK_MCP_UPLOAD_FILE_TOOL` (channel policy) |
| `attachment_get_data` | `SLACK_MCP_ATTACHMENT_TOOL` |
| `conversations_open` | `SLACK_MCP_OPEN_CONVERSATION_TOOL` |
| `conversations_mark` | `SLACK_MCP_MARK_TOOL` (user token required) |
| `conversations_join`, `conversations_leave` | `SLACK_MCP_JOIN_TOOL` |
| `conversations_rename` | `SLACK_MCP_RENAME_CHANNEL_TOOL` |
| `conversations_create` | `SLACK_MCP_CREATE_CHANNEL_TOOL` |
| `conversations_set_topic` | `SLACK_MCP_SET_TOPIC_TOOL` |
| `conversations_invite` | `SLACK_MCP_INVITE_TOOL` |
| `conversations_invite_shared` | `SLACK_MCP_INVITE_SHARED_TOOL` |
| `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `usergroups_me` join/leave | `SLACK_MCP_USERGROUPS_WRITE_TOOL` |

`usergroups_list` and `usergroups_me` (action `list`) are read-only and registered by default. Usergroups tools require the `usergroups:read` scope, plus `usergroups:write` for writes.

Channel policies are checked against the resolved target: `#name` and `@name` (in the request and in the policy) are resolved to IDs, IDs are compared case-insensitively, and a user ID target is mapped to the existing DM with that user, or refused when a list is configured, so a denied DM cannot be reached by user ID. A deny-list entry that cannot be resolved blocks the tool until it can. Refusals do not reveal the policy to the model.

`as_user=true` is refused unless `SLACK_MCP_ALLOW_AS_USER=true`. With only a user token configured, every action is taken as the user.

`files_upload` requires the `files:write` OAuth scope. It accepts one conversation ID, a filename, and either UTF-8 text or base64 file bytes; inline content is limited to 1 MiB, and `path` uploads a file of up to 64 MiB from the files folder. Its channel allowlist is checked after resolving channel names.


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
| empty/not set   | `false`/`0`/`no`/`off` | No                   | N/A                 |
| empty/not set   | `true`               | Yes                    | None                |
| empty/not set   | `C123,C456`          | Yes                    | Only listed channels |
| includes tool   | not set              | Yes                    | None                |
| includes tool   | `false`              | No                     | N/A                 |
| includes tool   | `C123,C456`          | Yes                    | Only listed channels |
| excludes tool   | any                  | No                     | N/A                 |
