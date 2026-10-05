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
   subscription ends. On start it drops sessions that ended while it was down
   and catches the rest up (SAC-6). Each session has its own delivery queue
   and worker, so a slow or hung session never stalls the Socket Mode event
   loop or other sessions (SAC-9). Started with a neutral environment in its
   state directory (SAC-5). State in `<home>/slack-agent-chat/` (0700): `state.json`,
   `listener.sock`, `listener.log`.
2. **CLI helpers** (`slack-mcp-server chat …`): `watch start|stop|status`,
   `channel create|invite`, `post`, `ack`, `relay-hook`.
3. **Skill** `slack-agent-chat` for Codex (`$CODEX_HOME/skills/`) and Claude
   (`~/.claude/skills/`), plus a `UserPromptSubmit` hook for `%agents:`.

## Delivery

- **Codex**: direct WebSocket to `<CODEX_HOME>/app-server-control/app-server-control.sock`;
  `initialize` → `thread/read`; `idle` → `turn/start`; `active` →
  `thread/turns/list` (desc, limit 1) → `turn/steer` with `expectedTurnId`;
  JSON-RPC rejection → re-read and retry (3 attempts). The daemon unloads a
  thread once no terminal is attached and it is idle, so `notLoaded` or an
  unknown thread → the session is gone; drop its subscription (SAC-10). Daemon
  unreachable → delivery fails (no 👀); the message is caught up on the next
  `watch start`. There is no `codex queue` fallback. Deterministic
  `clientUserMessageId` (UUIDv5 of session, channel, ts).
- **Claude**: connect to the session's `CLAUDE_CODE_MESSAGING_SOCKET`, write
  `{"type":"auth","token":…}` then `{"type":"user","message":{"role":"user","content":…}}`.
  Socket missing or refusing → the session is gone; drop its subscription.
- Every minute the listener checks each subscribed session the same way and
  drops gone ones, so it can idle-exit after sessions quit without `watch stop`.
  Between a Codex terminal quitting and the daemon unloading its thread, a
  message can still start an unwatched turn; `watch stop` before quitting
  avoids that window.
- After a successful delivery, the listener's bot adds 👀 (`eyes`) to the
  message and logs the method used (`turn/steer`, `turn/start`, `inbox`).

## Routing

- Private channels only by convention; channel membership is the allow-list.
- Ignore own messages and non-content subtypes (edits, deletes, joins).
- Mentions = `<@U…>` tokens and plain `@name` text that resolve to channel
  members (agents or people). Only the run of mentions that opens a message
  (separated by whitespace, `,:;&` or "and") routes it (CS-31). No leading
  mentions → every agent except the sender, even if the message mentions
  someone later; such a later mention marks a command for that agent alone.
  Leading mentions → only the agents among them, so a message that opens with
  only people (e.g. a reply to the user, SAC-11) or names that resolve to no
  channel member reaches no agent. Threads follow the same rule.
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
