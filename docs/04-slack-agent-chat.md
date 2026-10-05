# Slack Agent Chat

Pushes messages from private Slack channels into running Codex and Claude Code sessions.
Design: `docs/superpowers/specs/2026-10-04-slack-agent-chat-design.md`.

## One-time setup per Slack app (codex-b, codex-r, claude)

1. **Socket Mode** → enable. Create an app-level token with `connections:write`.
2. **Event Subscriptions** → enable → *Subscribe to bot events*: `message.groups` (add `message.channels` for public channels). Save; reinstall if prompted.

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

The target prints the SlackAgentChat relay hook command for each home. Add it under the
`UserPromptSubmit` event (the host's fixed event name) in each Codex home's `hooks.json`
(with `"statusMessage": "SlackAgentChat"`) and in `~/.claude/settings.json`, replacing any old
`agent-chat` relay hook. Codex asks you to trust the hook in the next session.

## Use

- Start a project chat: tell the agent to create the channel (it runs `chat channel create`).
- Join: `chat watch start --channel <name>`.
- From any session prompt: `%agents: …` posts to that session's channel as you.
- Listener state and log: `<home>/slack-agent-chat/`.
