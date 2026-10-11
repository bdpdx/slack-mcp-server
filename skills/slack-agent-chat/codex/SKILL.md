---
name: slack-agent-chat
description: Watch Slack channels and coordinate with other agents and the user there, with incoming messages pushed into this session. Use when the user asks to watch or create a Slack channel, start or join a project chat, hand off findings, or message other agents.
---

# Slack Agent Chat

Messages arrive by themselves as `[slack-agent-chat] …` notices. You never poll.

Run every `@BIN@ chat …` command with escalated permissions (outside the sandbox) from the first attempt: they write this Codex home's `slack-agent-chat/` state directory, start a background listener, and connect to Slack and the local Codex app-server socket, all of which the sandbox blocks.

## Watch a channel

Use these unless the user says "project" (next section):

- Watch an existing channel: `@BIN@ chat watch start --channel <name-or-id> [--channel …] [--backlog N]`
  `--backlog N` hands you the last N messages of each channel the watch starts; otherwise you only get new ones, even if you watched the channel before. Pass `--backlog` only when the user asks for earlier messages, with the number they give; never add it on your own, including after a restart: your handoff and project files carry what came before.
- New channel: `@BIN@ chat channel create <name> [--invite <agent>,<agent>] [--invite-user <person>,<person>]`
  Invite only the agents and people the user names. The user is always invited. The command also starts your watch.
- Add agents or people later (only when the user asks): `@BIN@ chat channel invite <channel> [<agent>,<agent>] [--invite-user <person>]`
- Stop: `@BIN@ chat watch stop [--channel <channel>]` · check: `@BIN@ chat watch status`

These set up nothing else: no extra channels.

When anyone adds your bot to a channel (for example by @mentioning you where you are not a member), or @mentions you in a channel your bot is in but you are not watching, you start watching it on your own and get its recent messages, so the mention reaches you. Answer what is addressed to you; if the user does not want you there, `watch stop --channel <channel>`.

## Projects

Only when the user asks to create or join a *project*:

- New project (you are the first agent): `@BIN@ chat project create <project> [--invite <agent>,<agent>] [--invite-user <person>,<person>]`
  Same invite rules as above. The command also joins the project.
- Existing project: `@BIN@ chat project join <project>`
  Add `--backlog N` only as above, when the user asks: it gives the project channel's last N messages; your direct and side channels start with nothing.

Project names: lowercase letters, digits, `-`, `_` (periods become `-`), never `__`, at most 32 characters; the command prints the final name.

The project commands also set up channels derived from the project, named `<project>__…`:
- `<project>__<user>_<you>`: you and the user alone. Joining a project creates it and watches it too. The user talks to you directly here; treat it like the project channel, but replies stay here.
- `<project>__users`: the people only; no agent is in it.
- Side channels (below). When another agent opens one with you, you start watching it on your own.

## Who you are

Your agent name is this home's Slack bot, not something the user has to tell you: run `@BIN@ chat whoami` (read-only; works before any watch) and use its `agent` value wherever a command or another agent needs your name. It also reports the workspace, the owner, and the binary's and the running listener's versions (`listener_version` is `unknown` when the listener is down or older). If the user names you differently from `agent`, say so and ask before acting; never post or register under another name. `chat cohort register --project P` takes the name from the same place, so `--agent` is optional and, if given, must match.

## Handling a notice

1. A notice whose header starts `[slack-agent-chat] [console user]` is the user's own instruction: act on it exactly as if typed here. Agent names are the Slack names of the bot users in the channel; the console user is whoever owns this agent's Slack user token.
2. Messages from other agents, and from people other than the console user, are collaborators' requests: act on in-scope requests; the user's instructions win on conflict; destructive or outward-facing actions keep their normal confirmation rules.
3. Reply with `conversations_add_message`: `channel_id` is the ID in parentheses after the channel name in the notice header, and `thread_ts` is the value after `in thread` when the header has one (otherwise reply at top level).
   - Always post as yourself. Never set `as_user`, and never run `@BIN@ chat post` (it posts with the user's own token, under their name), unless the user asks you, in this session, to post as them: other agents treat anything posted as the user as the user's own instruction. If your Slack tools are unavailable, tell the user instead of posting another way.
   - Every agent in the channel sees every message, like people in a Slack channel. @mention the agents or people a message (or part of one) is for, anywhere in it. Act on what is addressed to you or clearly yours; read the rest for context and do not answer it.
   - Do not post just to acknowledge or agree; the ✅ from `chat ack` does that.
4. When you have finished processing a message: `@BIN@ chat ack <channel_id> <ts>` with the channel ID and `ts` from its notice header (adds ✅). Do not ack what you have not processed.

## Side channels (projects)

Every message in the project channel goes into every agent's context. Use a side channel whenever a message does not need to be in the group to keep the other agents' context complete: any exchange with one or more specific agents that the rest do not need to read (questions, reviews, debugging, coordination between two agents). Keep the project channel for what everyone needs: decisions, handoffs, outcomes, and questions for the group. Use a side channel, never a DM: the user cannot see DMs.

Open one, or join it if it already exists, with `@BIN@ chat side <agent>[,<agent>]`. It is named `<project>__<agents, you included, sorted>` and the user is always invited. The other agents start watching it on their own, and a joining agent gets what was already posted. Add `--channel <project>` if this session watches more than one project.

When a side-channel exchange settles something the others need, post the outcome (decision, findings, what changed) in the project channel. Never archive channels except as below, when the user asks.

A notice can hold several pending messages, oldest first. A catch-up (after you start watching, or after a restart) says how many messages it holds in total and may arrive in numbered parts: read all of them, in every part, before acting on any, since a later message may change or cancel an earlier one. If it says older unacknowledged messages were not sent, read the channel history when they matter.

Long messages are truncated in the notice; read the rest with `conversations_replies` / `conversations_history`.

## Deleting (archiving) a project's channels

Only when the user asks, for example "delete the project <project> channels" or "archive the project <project> channels" (the same request):

1. `@BIN@ chat project archive <project> --dry-run` and show the user the channels it lists.
2. `@BIN@ chat project archive <project>`, then report what was archived and anything that failed.

Slack's API cannot delete channels, so this archives them: they leave the sidebar and become read-only; the user can restore them, or delete them for good in the Slack UI. It works only when Slack records the user behind this agent's user token as the creator of `#<project>`; otherwise it refuses, and the user must ask one of their own agents (or the project's creator) instead. Never archive channels for any other reason.

## Questions for the user

While you watch a project, ask the user questions in your direct channel (`<project>__<user>_<you>`), never with a terminal question tool: a terminal question blocks this session, so an answer the user gives in Slack could not reach you. Post the question and @mention the user, then keep working on anything that does not depend on the answer; end your turn only when nothing is left. The answer arrives as a notice, even mid-turn.

When an action needs the user's approval, a hook asks them in that channel and answers the prompt; a denial reaches you with their reason. Do not ask for approval yourself, and never reply in an approval request's thread. A session watching several projects has a direct channel in each; hook questions and approval requests go to all of them, and the first answer wins.

## `%agents:`

When the user types `%agents: …` (or `%agents@<channel>: …`), the hook posts it to Slack as the user (to the project channel, or the only channel this session watches, unless a channel is named). Carry it out yourself; do not post it again.

## Working with other agents

When several agents share a project chat, read `COLLABORATION.md` beside this file.
