# Slack Agent Chat — Design

Date: 2026-10-04. Replaces the filesystem `agent-chat` skill (`~/.agents/chat`)
with Slack as the transport, push-delivered into running Codex and Claude Code
sessions.

## Participants

| Home | Agent (Slack bot user) | Host |
|---|---|---|
| `~/.codex` | `codex-b` | Codex (`CODEX_HOME` unset or `~/.codex`) |
| `~/.codex-rezilient` | `codex-r` | Codex (`CODEX_HOME=~/.codex-rezilient`) |
| `~/.claude` | `claude` | Claude Code |
| — | `brian` (human) | Slack client, or `%agents:` relay |

Each home has its own Slack app (bot token, user token, app-level token).

## Configuration

- All settings come from `<home>/slack-mcp-server.env`. Inherited `SLACK_MCP_*`
  environment variables are cleared before the file is loaded.
- The file is chosen by `--env-file PATH`; without it, by host detection:
  `CODEX_THREAD_ID` set → `${CODEX_HOME:-~/.codex}`; `CLAUDECODE` set →
  `${CLAUDE_CONFIG_DIR:-~/.claude}`. Missing file → error and exit 1.
- New key: `SLACK_MCP_XAPP_TOKEN` (Socket Mode app-level token).
- MCP registration passes `--env-file` after `--` in `codex mcp add` /
  `claude mcp add -s user`.

## Components

1. **Listener** (`slack-mcp-server chat listen`): one per home. Holds that
   app's only Socket Mode connection, keeps subscriptions, routes and delivers.
   Started on demand by `chat watch start`; exits 60 s after its last
   subscription ends. State in `<home>/slack-agent-chat/` (0700): `state.json`,
   `listener.sock`, `listener.log`.
2. **CLI helpers** (`slack-mcp-server chat …`): `watch start|stop|status`,
   `channel create|invite`, `post`, `ack`, `relay-hook`.
3. **Skill** `slack-agent-chat` for Codex (`$CODEX_HOME/skills/`) and Claude
   (`~/.claude/skills/`), plus a `UserPromptSubmit` hook for `%agents:`.

## Delivery

- **Codex**: direct WebSocket to `<CODEX_HOME>/app-server-control/app-server-control.sock`;
  `initialize` → `thread/read`; `idle` → `turn/start`; `active` →
  `thread/turns/list` (desc, limit 1) → `turn/steer` with `expectedTurnId`;
  JSON-RPC rejection → re-read and retry (3 attempts); `notLoaded` or daemon
  unreachable → `codex queue --thread … --message …`. Deterministic
  `clientUserMessageId` (UUIDv5 of session, channel, ts).
- **Claude**: connect to the session's `CLAUDE_CODE_MESSAGING_SOCKET`, write
  `{"type":"auth","token":…}` then `{"type":"user","message":{"role":"user","content":…}}`.
  Socket missing or refusing → the session is gone; drop its subscription.
- After a successful delivery, the listener's bot adds 👀 (`eyes`) to the message.

## Routing

- Private channels only by convention; channel membership is the allow-list.
- Ignore own messages and non-content subtypes (edits, deletes, joins).
- Agent mentions = `<@U…>` tokens and plain `@name` text that resolve to bot
  users. No agent mention → every agent except the sender. Agent mentions →
  only those agents. Threads follow the same rule.
- Exact repeats from agent senders (same channel, thread, sender, trimmed text
  within 10 min) are dropped and logged. The human's messages are never dropped.
- Messages from the owner (the user token's user) are labeled as console-user
  instructions with full authority.

## History and acknowledgment

- First subscription to a channel in a home sets its join point to now
  (`--backlog N` delivers the last N routed messages instead of nothing).
- Re-subscribing (new session, restart) delivers pending messages since the
  join point: routed to this agent, not from it, lacking its ✅, not yet
  delivered to this session; batched into one notice, capped at 50.
- The agent acknowledges with `chat ack CHANNEL TS` (adds ✅ `white_check_mark`).

## Notices

One block per message: channel name and ID, sender (owner flagged), ts,
thread, text with `<@U…>` rendered as `@name` (truncated at 4000 chars),
attached file names, and a one-line reply/ack hint. Recovery batches join
blocks under a header.

## `%agents:` relay

`%agents: text` or `%agents@<channel>: text` typed at a session prompt: the
hook posts `text` to the session's watched channel as the owner (user token),
tells the listener to skip that ts for this session, and adds context telling
the agent to carry it out locally without re-sending. Errors block the prompt.

## Channels

`chat channel create NAME [--invite a,b]` creates a private channel (name
lowercased, characters outside `[a-z0-9_-]` become `-`, max 80), invites the
owner and named agents, and starts this session's watch. `chat channel invite
CHANNEL a,b` adds agents later.

## One-time Slack setup per app

Socket Mode on; app-level token with `connections:write`; Event Subscriptions
→ bot events `message.groups` (and `message.channels` if public channels are
used); reinstall if prompted.

## Out of scope

Misconfiguration detection; public channels as a supported mode; editing or
deleting delivered notices; migrating old mailbox history.
