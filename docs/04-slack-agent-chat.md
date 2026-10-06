# Slack Agent Chat

Pushes messages from private Slack channels into running Codex and Claude Code sessions.
Design: `docs/superpowers/specs/2026-10-04-slack-agent-chat-design.md`.

## One-time setup per Slack app (codex-b, codex-r, claude)

1. **Socket Mode** → enable. Create an app-level token with `connections:write`.
2. **Event Subscriptions** → enable → *Subscribe to bot events*: `message.groups` and `member_joined_channel` (add `message.channels` for public channels).
3. **OAuth & Permissions** → *User Token Scopes*: add `groups:write` (the listener creates `<project>__users` and adds people to it as you). *Bot Token Scopes*: add `im:write` and `chat:write` if missing (turn-end DMs).
4. **Interactivity & Shortcuts** → On (approval buttons; Socket Mode needs no request URL).
5. Reinstall the app when prompted, and copy the new user token if it changed.

Each person (e.g. a teammate adding their own agents) creates their own apps; the user token
they install with is theirs, so their agents treat them as the console user and `%agents:`
posts as them. Usernames and agent names must not contain `.` or `_`, and no agent may be
named `users`.

## Env files

One per home, mode 0600:

```
~/.codex/slack-mcp-server.env
~/.codex-rezilient/slack-mcp-server.env
~/.claude/slack-mcp-server.env
```

Each holds that agent's settings, including:

```
SLACK_MCP_XOXB_TOKEN=xoxb-…
SLACK_MCP_XOXP_TOKEN=xoxp-…
SLACK_MCP_XAPP_TOKEN=xapp-…
SLACK_MCP_ADD_MESSAGE_TOOL=true
```

Environment variables are no longer read; the file is the only source. Without `--env-file`, the
server looks in the detected session's home (`${CODEX_HOME:-~/.codex}` inside Codex, `~/.claude`
inside Claude Code) and exits with an error if the file is missing.

## Register the MCP server

```bash
codex mcp add slack -- ~/.bin/slack-mcp-server --transport stdio --env-file ~/.codex/slack-mcp-server.env
CODEX_HOME=~/.codex-rezilient codex mcp add slack -- ~/.bin/slack-mcp-server --transport stdio --env-file ~/.codex-rezilient/slack-mcp-server.env
claude mcp add -s user slack -- ~/.bin/slack-mcp-server --transport stdio --env-file ~/.claude/slack-mcp-server.env
```

Remove the old `[mcp_servers.slack.env]` token block from each `config.toml`.

## Install skills

```bash
make install-agent-chat
```

This builds `./build/slack-mcp-server` and installs the skills pointing at `AGENT_CHAT_BIN`
(default `~/.bin/slack-mcp-server`), which must be, or link to, that build output.

The target prints the SlackAgentChat hook commands for each home. Add them to each Codex home's
`hooks.json` (with `"statusMessage": "SlackAgentChat"`) and to `~/.claude/settings.json`, replacing
any old `agent-chat` relay hook. Codex asks you to trust new hooks in the next session.

- `UserPromptSubmit` → `relay-hook`: `%agents:` prompts (both hosts).
- `PreToolUse` with matcher `AskUserQuestion` → `ask-hook` (Claude): questions go to the
  session's `#<project>__<you>_<agent>` channel instead of the terminal.
- `PermissionRequest` → `approval-hook`, with `"timeout": 660`. Claude runs it only when it would
  show a permission dialog. Codex runs it *before* its `auto_review` reviewer, so with
  `approvals_reviewer = "auto_review"` the hook would take over every request the reviewer would
  approve on its own; leave it out of such Codex homes. When installed: approval requests
  come to that channel with Allow / Deny / Answer in terminal buttons. Only the button can allow;
  a reply in the channel or the request's thread can deny (`no` plus a reason for the agent) or say
  `terminal`. With no answer in
  10 minutes (`--wait`), the terminal asks instead.
- `Stop` → `stop-hook` (both hosts): when a turn you started by typing at the terminal ends, the
  agent's bot DMs you its final response. Turns started by Slack messages, background work or
  subagents send nothing. This replaces a Codex `notify` push notification.

## Use

- Start a project chat: tell the agent to create the channel (it runs `chat channel create`).
  This also creates `#<project>__users` (people only) and `#<project>__<you>_<agent>`.
- Join: `chat watch start --channel <name>`; this also creates or joins `#<project>__<you>_<agent>`,
  where you can talk to that agent alone.
- Agents open side channels (`#<project>__<agent>_<agent>`) with `chat side`; you are always in them.
- From any session prompt: `%agents: …` posts to that session's project channel as you.
- Clean up: tell any of your agents "delete the project <project> channels" (or "archive …"). It archives `#<project>`
  and its `#<project>__…` channels (`chat project archive`); Slack's API cannot delete channels, but
  you can delete archived ones in the Slack UI. Only the project's creator (through their own agents)
  can do this.
- Listener state and log: `<home>/slack-agent-chat/`.
