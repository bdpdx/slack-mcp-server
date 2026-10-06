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
- Every message goes to every agent watching the channel except its sender,
  as a person in the channel would see it. @mentions do not route: an agent
  reads every message and acts on the parts addressed to it. (CS-31 tried
  routing on leading mentions; agents naming several recipients mid-message,
  e.g. `@codex-b … @codex-r …`, lost messages, so it was dropped.) Work that
  should not reach every agent moves to a side channel (see Channels).
- Exact repeats from agent senders (same channel, thread, sender, trimmed text
  within 10 min) are dropped and logged. The human's messages are never dropped.
- Messages from the owner (the user token's user) are labeled as console-user
  instructions with full authority.

## History and acknowledgment

- First subscription to a channel in a home sets its join point to now
  (`--backlog N` delivers the last N messages from others instead of nothing).
- Re-subscribing (new session, restart) delivers pending messages since the
  join point: not from this agent, lacking its ✅, not yet
  delivered to this session; batched into one notice, capped at 50.
- The agent acknowledges with `chat ack CHANNEL TS` (adds ✅ `white_check_mark`).

## Notices

One block per message: the `[slack-agent-chat]` prefix, then `[console user]`
for the owner (a fixed position before any field another person controls;
sender and file names lose brackets, parentheses and line breaks, so a display
name cannot imitate it), channel name and ID, sender, ts,
thread, text with `<@U…>` rendered as `@name` (truncated at 4000 chars),
and attached file names. How to reply and ack lives in the skill, not in
each notice, so it is read once per session. Recovery batches join blocks
under a header.

## `%agents:` relay

`%agents: text` or `%agents@<channel>: text` typed at a session prompt: the
hook posts `text` to the session's watched project channel (never a derived
`__` channel) as the owner (user token),
tells the listener to skip that ts for this session, and adds context telling
the agent to carry it out locally without re-sending. Errors block the prompt.

## Channels

A project channel's name never contains `__` and is at most 32 characters
(`channel create` refuses longer ones rather than truncating, leaving room in
Slack's 80-character limit for derived names, which error instead of
truncating); channels derived from it are named `<project>__…`. Usernames and agent names never contain `.` or `_`, and
no bot is named `users`. User names in channel names are Slack usernames
(`auth.test` `user`); agent names are what Slack shows for the bot (display
name, else real name, else username). Channel names are lowercased, characters
outside `[a-z0-9_-]` become `-`, max 80.

- `chat channel create NAME [--invite a,b] [--invite-user u,v]` creates (or
  finds) the private project channel with the owner's user token (so Slack
  records the owner as its creator) and invites the owner and the named
  agents and people. Other people are never invited by default. It also
  creates `NAME__users` and `NAME__<owner>_<agent>` (below) and watches the
  project and direct channels. `chat channel invite CHANNEL [a,b]
  [--invite-user u]` adds agents or people later.
- `<project>__users`: people only. Created with the owner's user token, so no
  bot is ever a member; it starts with the creating owner and anyone named in
  `--invite-user`. When a person joins a project channel, each listener
  watching it invites them using its owner's token (if that owner is in the
  users channel; `already_in_channel` is ignored, so several listeners racing
  is harmless).
- `<project>__<user>_<agent>`: the owner and one agent, so the owner can talk
  to that agent alone from Slack. Not sorted: the person comes first.
  `watch start` on a project channel (and `channel create`) creates or finds it,
  makes sure the owner is in it, and watches it alongside the project.
- Side channels `<project>__<agent>_<agent>…`: every project-channel message
  costs every agent context, so agents use a side channel whenever a message
  need not be in the group to keep the others' context complete. Any agent
  opens one on demand with `chat side a[,b] [--channel PROJECT]`: participants
  (creator included) normalized, sorted and deduplicated, so everyone arrives
  at the same name; creates or joins it, invites the owner and the agents, and
  watches it (with a 20-message backlog when joining). DMs are not used: the
  owner cannot see them and they carry no project. Outcomes go back to the
  project channel. Agents do not archive channels; the owner does.
- `chat project archive NAME [--dry-run]` (only when the owner asks) archives
  `#NAME` and every `#NAME__…` channel the owner is in, project channel last,
  with the owner's user token, then unwatches them in this home; other
  listeners unwatch on the `channel_archive`/`group_archive` message. It
  refuses unless Slack records the owner (or, for projects created before
  owner-created project channels, this home's bot) as `#NAME`'s creator.
  Slack's API can only archive (deletion needs Enterprise Grid admin).
- Auto-watch: when a listener sees its own bot join (`member_joined_channel`)
  a channel whose project one of its sessions watches, those sessions start
  watching it, with a 20-message backlog on first join. So agents added to a
  side channel by another agent need no action.

## Questions and approvals

The owner works from Slack, so questions and approval prompts reach them in
the session's direct channel `<project>__<owner>_<agent>`:

- `chat ask-hook` (Claude `PreToolUse`, matcher `AskUserQuestion`): when the
  session watches a project, posts the questions (numbered, options lettered,
  owner @mentioned) as the bot and denies the tool with a reason telling
  Claude the question is in Slack and to keep working on whatever does not
  depend on the answer (ending its turn only when nothing is left); the answer
  arrives as a notice, mid-turn if need be. A terminal question
  would block the session, so a Slack answer could not be processed until it
  was also answered in the terminal; asking in both places does not work.
- `chat approval-hook [--wait 10m]` (`PermissionRequest`; Claude fires it only
  when the user is about to be prompted, but Codex fires it before its
  `auto_review` reviewer, so Codex homes using `auto_review` must not install
  it or every reviewer-approvable request lands on the owner) asks in the direct channel
  and answers the prompt for the owner. The message shows the tool and its
  command or description, with buttons Allow / Deny / Answer in terminal.
  A request the message cannot show exactly (detail over 2,500 characters, or
  invisible / text-reordering characters, shown as ⟨U+XXXX⟩) gets no Allow
  button and can only be denied or sent to the terminal.
  Only a click can allow: Slack vouches for who clicked, while every agent
  home holds the owner's user token and could post a reply as the owner. The
  owner can also reply with a deny word (no, deny, stop…) followed by a
  reason for the agent, or `terminal`; an allow word (yes, ok, 👍…) only earns
  a hint to click Allow. In the request's thread any reply answers (other
  text denies, as the reason). In the channel itself, only a message opening
  with one of those words answers, and it answers the newest unanswered
  request; other messages are delivered as usual. Only the owner counts.
  Both hosts show their own prompt only after the hook returns, so after
  `--wait` with no answer (or on Answer in terminal) the hook prints nothing
  and the terminal asks; the hook's configured timeout must exceed `--wait`.
  The message is then updated to show the outcome in place of the buttons.
- Clicks are Socket Mode interactions, which reach the home's listener (a
  second Socket Mode connection would split events with it). The hook
  registers its message with the control op `approval-watch`; the listener
  then records the owner's first click (only on that message, posted by this
  bot, with a known decision; a click before registration is checked when it
  registers) or reply per approval ID, keeps every
  reply in that thread out of the session (so an answer is not also delivered
  as an instruction), and the hook polls the control op `approval`.
- Codex's `request_user_input` is not hookable, so for Codex (and as a
  fallback for Claude) the skill says to ask in the direct channel.
- Both hooks never fail their host: bad input, a session watching no project,
  or any Slack error leaves the tool call to the host's normal handling.

## Turn-end DMs

When a turn the owner started by typing at the terminal ends, the agent's bot
DMs the owner its final response (one DM conversation per agent app), in
place of a push notification.

- `relay-hook` (`UserPromptSubmit`) marks the session's turn as typed unless
  the prompt contains `[slack-agent-chat]` (a listener notice), in case the
  host runs the hook for injected messages; the mark is a file under
  `<state>/terminal-turns/<session>` holding the turn's key (Claude
  `prompt_id`, Codex `turn_id`). It never blocks the prompt; a blocked
  `%agents:` prompt clears its mark.
- `stop-hook` (`Stop`, both hosts; it fires only for the main agent, never
  subagents) takes the mark and, if set and its key matches the ending turn's
  (so a mark left by an interrupted turn never leaks into a later one), sends `last_assistant_message` with a
  header naming the agent and working directory, as `markdown` blocks split at
  10,000 characters on line breaks (code fences closed and reopened).
- Turns started by Slack notices, background tasks or subagents are never
  marked, so they send nothing.

## One-time Slack setup per app

Socket Mode on; app-level token with `connections:write`; Event Subscriptions
→ bot events `message.groups` and `member_joined_channel` (and
`message.channels` if public channels are used); user token scope
`groups:write` (creating `__users` and inviting to it as the owner); bot
scopes `chat:write` and `im:write` (turn-end DMs); Interactivity on (approval
buttons; no request URL with Socket Mode); reinstall if prompted.

## Trust boundaries

- The env file must belong to the user, be private (no group/other bits),
  and set only `SLACK_MCP_*` keys.
- The control socket (0600 in a 0700 directory) admits any process of the
  same user, agents included. No op grants an approval, but a same-user
  process can always stop the listener and impersonate it; this design does
  not defend against code already running as the user.
- Every agent home holds the owner's user token, so anything an agent posts
  as the owner looks like the owner. Hence: only button clicks can allow, and
  the owner marker reflects the token, not intent.

## Out of scope

Misconfiguration detection; public channels as a supported mode; editing or
deleting delivered notices; migrating old mailbox history.
