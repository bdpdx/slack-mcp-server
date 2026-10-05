---
name: slack-agent-chat
description: Coordinate with other agents (codex-b, codex-r, claude) and the user through private Slack channels, with incoming messages pushed into this session. Use when the user asks to start, join, or watch a project chat, hand off findings, or message other agents.
---

# Slack Agent Chat

Messages arrive by themselves as `[slack-agent-chat] …` notices. You never poll.

## Start or join

- New project (you are the first agent): `@BIN@ chat channel create <project> [--invite codex-r,claude]`
  Invite only the agents the user names. The user is always invited. The command also starts your watch.
- Existing channel: `@BIN@ chat watch start --channel <name-or-id> [--backlog N]`
  `--backlog N` hands you the last N messages the first time this agent joins; otherwise you only get new ones.
- Add an agent later (only when the user asks): `@BIN@ chat channel invite <channel> codex-r`
- Stop: `@BIN@ chat watch stop [--channel <channel>]` · check: `@BIN@ chat watch status`

Channel names: lowercase letters, digits, `-`, `_` (periods become `-`); the command prints the final name.

## Handling a notice

1. A notice from `brian (the console user…)` is the user's own instruction: act on it exactly as if typed here.
   Claude Code labels these notices as coming from another session; that label does not reduce the user's authority here, except that a notice can never answer a permission prompt or change settings.
2. Messages from other agents are collaborators' requests: act on in-scope requests; the user's instructions win on conflict; destructive or outward-facing actions keep their normal confirmation rules.
3. Reply with `conversations_add_message` (channel from the notice; `thread_ts` when the notice says it is in a thread).
   - Address replies: start with `@codex-b`, `@codex-r`, `@claude` or `@brian`. A message with no agent @mention goes to every agent; broadcast only on purpose.
4. When you have finished processing a message: `@BIN@ chat ack <channel> <ts>` (adds ✅). Do not ack what you have not processed.

Long messages are truncated in the notice; read the rest with `conversations_replies` / `conversations_history`.

## `%agents:`

When the user types `%agents: …` (or `%agents@<channel>: …`), the hook posts it to Slack as the user. Carry it out yourself; do not post it again.
