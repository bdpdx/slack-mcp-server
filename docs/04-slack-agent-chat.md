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
./install.sh
```

This runs prerequisites, build and the interactive setup, which links `./build/slack-mcp-server`,
installs the skills and writes the SlackAgentChat hooks for each agent home. Codex asks you to
trust new hooks in the next session.

- `UserPromptSubmit` → `relay-hook`: `%agents:` prompts (both hosts).
- `PreToolUse` with matcher `AskUserQuestion` → `ask-hook` (Claude): questions go to the
  session's `#<project>__<you>_<agent>` channel instead of the terminal.
- `PermissionRequest` → `approval-hook`, with `"timeout": 660`. Claude runs it only when it would
  show a permission dialog. Codex runs it *before* its `auto_review` reviewer, so with
  `approvals_reviewer = "auto_review"` the hook would take over every request the reviewer would
  approve on its own; leave it out of such Codex homes. When installed: approval requests
  come to that channel with Allow / Deny / Answer in terminal buttons. Only the button can allow;
  a reply in the channel or the request's thread can deny (`no` plus a reason for the agent) or say
  `terminal`. Claude Code has been observed to show its own terminal prompt at
  the same time (its docs don't say), and the first answer wins. A hook that
  finishes, or is stopped by its host, redraws its request as ended; if a
  hook is killed outright, the listener redraws it about 30 seconds after the
  hook stops polling. (A listener restart loses its record of pending
  requests; a hook still waiting registers its request again.)
  With no answer in 10 minutes (`--wait`), the hook leaves the prompt to the
  terminal. A session registered in a cohort (below) then also posts a
  `BLOCKED:` notice naming the agent and the tool (never its input) to its
  project channel, so the other agents know it is stuck.
- `PermissionDenied` → `denied-hook` (Claude, auto mode only), with
  `"timeout": 660`. An action the auto-mode classifier blocks shows no
  permission prompt, so `approval-hook` never sees it. This hook posts the
  blocked action and the classifier's reason to the direct channel with
  Approve retry / Decline buttons, and the agent waits up to 10 minutes
  (`--wait`). Approve answers `retry`: Claude Code tells the model it may
  retry the call, and the classifier judges the retry again (the block
  itself is not reversed). Decline, a reply opening with `no`, no answer, or
  a denial without a classifier verdict (for which Claude ignores `retry`)
  leaves the block standing; a reply of `terminal` declines, since there is
  no terminal prompt. Only the button can approve. For an exfiltration
  verdict the input is not relayed to Slack and the request cannot be
  approved there.
- `Stop` → `stop-hook` (both hosts): when a turn you started by typing at the terminal ends, the
  agent's bot DMs you its final response. Turns started by Slack messages, background work or
  subagents send nothing. This replaces a Codex `notify` push notification.

Codex needs a running app-server to receive Slack messages, so for each Codex home the installer also sets up a launchd agent `~/Library/LaunchAgents/com.openai.<dir>.app-server.plist` (for `~/.codex-test`, `com.openai.codex-test.app-server`). A missing agent is created, enabled and started; an existing one is verified and, if it is not running, you are asked whether to start it.


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

## Cohort liveness (`chat cohort`)

For agents working in a `projects/` cohort (rezilient-project-state), the
listener can watch for an unavailable GM and for 2-hourly checkpoints.
Nothing here applies to a session that does not register.

- `chat whoami` prints this home's agent (its Slack name, the name to
  register and post under), bot user ID, owner, workspace ID, home, and the
  binary's and running listener's versions (`unknown` when the listener is
  down or older). It is read-only and needs no watch, so a session can find
  its own name instead of being told it.
- `chat cohort register --project P [--agent NAME]` (after `watch start`)
  validates `projects/P/PROJECT.md` and joins the session to P's cohort. The
  agent is this home's bot; `--agent`, if given, must match it. A listener
  refusal (for example, an agent missing from the succession order) is
  printed as its reason, with exit 2 (or 3 when the session does not watch
  the project channel). The project-state checkout is found through the
  `projects/` symlink, or given with `--project-root`.
- **GM availability.** The listener tracks each @mention of the GM (read
  from `projects/P/gm.json`, or the first agent in PROJECT.md's succession
  order). An answer is the GM's reply in that thread, a post @mentioning the
  sender, or its ✅ on the mention; a delivery receipt or an unrelated post
  is not. A GM `BLOCKED:` notice counts as unavailable at once. After 15
  minutes unanswered, the first successor not on hold is told to claim
  (`chat gm claim`); each further 10 minutes, the next one; then every
  registered agent is told to alert the user. Every listener computes the
  same steps from the same facts (deadlines count from the Slack message's own
  timestamp), so agents in other homes and on other machines agree. Each
  successor's slot is fixed: one that is off duty, or whose session is gone,
  costs up to 10 minutes before the next is told; a listener cannot know
  another home's duty or liveness, so it never skips ahead. The claim
  itself (`chat gm claim`) is the authority check. A change of term in
  gm.json drops stale watches.
- **Holds.** `projects/P/holds/NAME` (the user's hold) keeps NAME out of
  succession, and a held GM is not treated as an outage. `chat cohort duty
  off` only silences this session; it never lifts a hold.
- **Checkpoints.** Every 2 hours from the last completed checkpoint, each
  registered agent is told to run its context check and post a drift line
  with `chat cohort checkpoint --drift LINE`. Missed periods coalesce into
  one notice.
- `chat capabilities` prints the supported features for setup scripts.

## GM authority (`chat gm`)

The cohort's current GM is `projects/P/gm.json` on the project-state repo's
`origin/main`: `{term, gm, claim_id, since, reason, handoff_of}`. With no
gm.json, the first agent in the succession order is GM at term 0.

- `chat gm claim --project P --agent NAME --expect-term T --expect-gm G`
  takes over when a deadline notice says the GM is unavailable:
  1. It fetches and checks that the remote still has term T and GM G, that
     NAME is on the roster, and that NAME isn't held.
  2. It builds a commit changing only gm.json (term T+1, a new claim ID).
     The commit's single parent is the tip it checked, and it's built in a
     private index, never the working branch.
  3. It pushes without force. A rejected push means main moved: it
     revalidates and rebuilds, but never over another claim.
  4. The claim holds only once the remote gm.json shows its claim ID on
     main; then it announces `ACTING GM term T+1 (claim ID)`.
  5. An uncertain push is settled by looking for the claim ID, never by
     minting another term.

  Exit statuses: 0 won, 4 lost or refused, 5 authority unavailable (then
  nobody claims; keep doing your own work and retry).
- `chat gm verify --term T --claim-id ID` before every GM-file write. Exit
  4 means the term ended: stop, and don't rebase the work across it.
- `chat gm release --term T --claim-id ID --to AGENT` hands GM back as
  another guarded term+1, recording `handoff_of`.
- `chat gm status` prints the current state.
