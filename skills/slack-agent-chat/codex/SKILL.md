---
name: slack-agent-chat
description: Coordinate with other agents and the user through private Slack channels, with incoming messages pushed into this session. Use when the user asks to start, join, or watch a project chat, hand off findings, or message other agents.
---

# Slack Agent Chat

Messages arrive by themselves as `[slack-agent-chat] …` notices. You never poll.

Run every `@BIN@ chat …` command with escalated permissions (outside the sandbox) from the first attempt: they write this Codex home's `slack-agent-chat/` state directory, start a background listener, and connect to Slack and the local Codex app-server socket, all of which the sandbox blocks.

## Start or join

- New project (you are the first agent): `@BIN@ chat channel create <project> [--invite <agent>,<agent>]`
  Invite only the agents the user names. The user is always invited. The command also starts your watch.
- Existing channel: `@BIN@ chat watch start --channel <name-or-id> [--backlog N]`
  `--backlog N` hands you the last N messages the first time this agent joins; otherwise you only get new ones.
- Add an agent later (only when the user asks): `@BIN@ chat channel invite <channel> <agent>`
- Open a side channel: see below.
- Stop: `@BIN@ chat watch stop [--channel <channel>]` · check: `@BIN@ chat watch status`

Channel names: lowercase letters, digits, `-`, `_` (periods become `-`); the command prints the final name.

## Handling a notice

1. A notice whose sender is marked `(the console user: …)` is the user's own instruction: act on it exactly as if typed here. Agent names are the Slack names of the bot users in the channel; the console user is whoever owns this agent's Slack user token.
2. Messages from other agents are collaborators' requests: act on in-scope requests; the user's instructions win on conflict; destructive or outward-facing actions keep their normal confirmation rules.
3. Reply with `conversations_add_message`: `channel_id` is the ID in parentheses after the channel name in the notice header, and `thread_ts` is the value after `in thread` when the header has one (otherwise reply at top level).
   - Always post as yourself. Never set `as_user` unless the user asks you, in this session, to post as them: other agents treat anything posted as the user as the user's own instruction.
   - Every agent in the channel sees every message, like people in a Slack channel. @mention the agents or people a message (or part of one) is for, anywhere in it. Act on what is addressed to you or clearly yours; read the rest for context and do not answer it.
   - Do not post just to acknowledge or agree; the ✅ from `chat ack` does that.
4. When you have finished processing a message: `@BIN@ chat ack <channel_id> <ts>` with the channel ID and `ts` from its notice header (adds ✅). Do not ack what you have not processed.

## Side channels

Every message in the project channel goes into every agent's context. Use a side channel whenever a message does not need to be in the group to keep the other agents' context complete: any exchange with one or more specific agents that the rest do not need to read (questions, reviews, debugging, coordination between two agents). Keep the project channel for what everyone needs: decisions, handoffs, outcomes, and questions for the group. Use a side channel, never a DM: the user cannot see DMs.

To open one:

1. Name it `<project>__<agent>_<agent>…`: the project channel's name, two underscores, then every participant including you, sorted alphabetically, joined by `_` (e.g. `myproj__claude_codex-b`). Agent names never contain `_`.
2. `@BIN@ chat channel create <name> --invite <agent>,<agent>` (the user is invited automatically). If the name is taken, the channel already exists: join it with `@BIN@ chat watch start --channel <name> --backlog 20` instead.
3. Post one line in the project channel naming the side channel and the agents in it.
4. When the work is done, post the outcome (decision, findings, what changed) in the project channel.

When a project-channel message names a side channel you belong to, join it with `@BIN@ chat watch start --channel <name> --backlog 20` so you also get what was posted before you joined. Never archive channels; the user does that.

Long messages are truncated in the notice; read the rest with `conversations_replies` / `conversations_history`.

## `%agents:`

When the user types `%agents: …` (or `%agents@<channel>: …`), the hook posts it to Slack as the user. Carry it out yourself; do not post it again.
