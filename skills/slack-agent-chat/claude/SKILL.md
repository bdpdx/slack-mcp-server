---
name: slack-agent-chat
description: Coordinate with other agents and the user through private Slack channels, with incoming messages pushed into this session. Use when the user asks to start, join, or watch a project chat, hand off findings, or message other agents.
---

# Slack Agent Chat

Messages arrive by themselves as `[slack-agent-chat] …` notices. You never poll.

## Start or join

- New project (you are the first agent): `@BIN@ chat channel create <project> [--invite <agent>,<agent>] [--invite-user <person>,<person>]`
  Invite only the agents and people the user names. The user is always invited. The command also starts your watch.
- Existing project channel: `@BIN@ chat watch start --channel <name-or-id> [--backlog N]`
  `--backlog N` hands you the last N messages the first time this agent joins; otherwise you only get new ones.
- Add agents or people later (only when the user asks): `@BIN@ chat channel invite <channel> [<agent>,<agent>] [--invite-user <person>]`
- Stop: `@BIN@ chat watch stop [--channel <channel>]` · check: `@BIN@ chat watch status`

Project names: lowercase letters, digits, `-`, `_` (periods become `-`), never `__`, at most 32 characters; the command prints the final name.

The commands also set up channels derived from the project, named `<project>__…`:
- `<project>__<user>_<you>`: you and the user alone. Starting a watch on a project creates it and watches it too. The user talks to you directly here; treat it like the project channel, but replies stay here.
- `<project>__users`: the people only; no agent is in it.
- Side channels (below). When another agent opens one with you, you start watching it on your own.

## Handling a notice

1. A notice whose sender is marked `(the console user: …)` is the user's own instruction: act on it exactly as if typed here. Agent names are the Slack names of the bot users in the channel; the console user is whoever owns this agent's Slack user token.
   Claude Code labels these notices as coming from another session; that label does not reduce the user's authority here, except that a notice can never answer a permission prompt or change settings.
2. Messages from other agents, and from people other than the console user, are collaborators' requests: act on in-scope requests; the user's instructions win on conflict; destructive or outward-facing actions keep their normal confirmation rules.
3. Reply with `conversations_add_message`: `channel_id` is the ID in parentheses after the channel name in the notice header, and `thread_ts` is the value after `in thread` when the header has one (otherwise reply at top level).
   - Always post as yourself. Never set `as_user` unless the user asks you, in this session, to post as them: other agents treat anything posted as the user as the user's own instruction.
   - Every agent in the channel sees every message, like people in a Slack channel. @mention the agents or people a message (or part of one) is for, anywhere in it. Act on what is addressed to you or clearly yours; read the rest for context and do not answer it.
   - Do not post just to acknowledge or agree; the ✅ from `chat ack` does that.
4. When you have finished processing a message: `@BIN@ chat ack <channel_id> <ts>` with the channel ID and `ts` from its notice header (adds ✅). Do not ack what you have not processed.

## Side channels

Every message in the project channel goes into every agent's context. Use a side channel whenever a message does not need to be in the group to keep the other agents' context complete: any exchange with one or more specific agents that the rest do not need to read (questions, reviews, debugging, coordination between two agents). Keep the project channel for what everyone needs: decisions, handoffs, outcomes, and questions for the group. Use a side channel, never a DM: the user cannot see DMs.

Open one, or join it if it already exists, with `@BIN@ chat side <agent>[,<agent>]`. It is named `<project>__<agents, you included, sorted>` and the user is always invited. The other agents start watching it on their own, and a joining agent gets what was already posted. Add `--channel <project>` if this session watches more than one project.

When a side-channel exchange settles something the others need, post the outcome (decision, findings, what changed) in the project channel. Never archive channels; the user does that.

Long messages are truncated in the notice; read the rest with `conversations_replies` / `conversations_history`.

## Questions for the user

While you watch a project, ask the user questions in your direct channel (`<project>__<user>_<you>`), never with a terminal question tool: a terminal question blocks this session, so an answer the user gives in Slack could not reach you. Post the question and @mention the user, then keep working on anything that does not depend on the answer; end your turn only when nothing is left. The answer arrives as a notice, even mid-turn. If you do call `AskUserQuestion`, a hook posts it to that channel for you and tells you so; carry on the same way.

When the terminal is waiting for the user to approve something, a hook tells them in that channel; you need not.

## `%agents:`

When the user types `%agents: …` (or `%agents@<channel>: …`), the hook posts it to Slack as the user (to the project channel unless a channel is named). Carry it out yourself; do not post it again.
