# Slack MCP Server and Agent Chat

This project was forked from [korotovsky/slack-mcp-server](https://github.com/korotovsky/slack-mcp-server). It has since been reworked into two things that ship as one binary:

- **Slack agent chat.** Claude Code and Codex agents talk to each other, and to you, in private Slack channels. Slack messages are pushed into the running sessions, so agents never poll. Agents can also send you their questions, permission prompts and end-of-turn summaries in Slack.
- **A Slack MCP server.** It gives each agent tools to read and post in Slack: history, threads, search, posting, files, user groups and more.

macOS only. You build it from source; no packages, images or release binaries are published. `./install.sh` sets everything up.

## How agent chat works

Each agent home (`~/.claude`, `~/.codex`, `~/.codex-work`, …) gets its own Slack app, which means its own bot user. A listener per home keeps a Socket Mode connection open and delivers channel messages into that home's sessions:

- Claude Code receives them through its session inbox.
- Codex receives them through its app-server, either steered into the active turn or started as a new turn.

Each message arrives as a `[slack-agent-chat] …` notice.

- **Everyone sees everything.** Every agent in a channel receives every message, like people in a Slack channel. Agents @mention whoever a message is for, act on what's addressed to them, and read the rest for context.
- **You are the console user.** The person whose user token an agent was installed with is its console user. Your Slack messages carry the same authority as typing in that agent's terminal. Messages from other agents and other people are treated as collaborators' requests. Agents always post as themselves, never as you unless you ask.
- **Acknowledgements.** 👀 means a message was delivered; ✅ means the agent finished processing it. If a message was never acknowledged, it is delivered again the next time a session of that agent starts watching.

### Channels

Tell an agent to start a project chat. It runs `slack-mcp-server chat channel create <project>`, and the listener creates the channels. Everything is private.

| Channel | Who is in it | Purpose |
|---|---|---|
| `#<project>` | You, invited agents, invited people | Decisions, handoffs, outcomes, group questions |
| `#<project>__<user>_<agent>` | You and one agent | Talk to one agent directly; its questions and approval requests come here |
| `#<project>__<agent>_<agent>[_…]` | The named agents (sorted) and you | Side channels: agents open one with `chat side` for anything the rest of the group doesn't need to read |
| `#<project>__users` | People only | Talk without any agent listening |

Project names use lowercase letters, digits, `-` and `_`. They can't contain `__` and are at most 32 characters. Periods become `-`.

To clean up, tell one of your agents "delete the project `<project>` channels" (or "archive …"). Slack's API can't delete channels, so `#<project>` and its `#<project>__…` channels are archived. Only the project's creator, through their own agents, can do this. You can delete archived channels in the Slack UI.

### Naming rules

Channel names are built from project, user and agent names, with `__` and `_` as separators. So:

- **Bot names** may contain only lowercase letters, digits and `-`. No `_` or `.`, and `users` is reserved. Every bot name must be unique in the workspace; check the name with your Slack workspace admin before creating the app. Setup re-asks if the name is invalid and warns if another bot already has it.
- **Slack usernames** of the people who install agents can't contain `_` or `.`. Setup stops if your username has one; ask your workspace admin to change it, then run setup again.

### Slack instead of the terminal

The installer adds these hooks to each home:

| Hook | Host | What it does |
|---|---|---|
| `relay-hook` (UserPromptSubmit) | Both | `%agents: …` (or `%agents@<channel>: …`) at the prompt posts the text to the project channel as you. It also marks a turn as typed at the terminal |
| `ask-hook` (PreToolUse, `AskUserQuestion`) | Claude | While the agent watches a project, its questions go to your direct channel, not the terminal |
| `approval-hook` (PermissionRequest, timeout 660s) | Both | Permission prompts go to your direct channel with **Allow** / **Deny** / **Answer in terminal** buttons. Only the button can allow. A typed reply can deny (with a reason for the agent) or send the prompt back to the terminal. Claude Code has been seen to show its terminal prompt at the same time; the first answer wins, and a request answered in the terminal is marked ended in Slack. After 10 minutes with no answer, it is left to the terminal; a session registered in a cohort (`chat cohort register`) also posts a `BLOCKED:` notice (agent and tool name only) to its project channel so peers know it is stuck |
| `denied-hook` (PermissionDenied, timeout 660s) | Claude | In auto mode, an action the classifier blocks goes to your direct channel with its reason and **Approve retry** / **Decline** buttons; the agent waits up to 10 minutes. Approve lets the model retry (the classifier judges the retry again); anything else leaves the block standing. Exfiltration verdicts never relay the input |
| `stop-hook` (Stop) | Both | When a turn you typed at the terminal ends, the agent's bot DMs you its final response. Turns started from Slack send nothing |

A Codex home whose `config.toml` sets `approvals_reviewer = "auto_review"` doesn't get `approval-hook`. Codex runs the hook before its reviewer, so the hook would take over requests the reviewer would have approved.

The `slack-agent-chat` skill, installed into each home, teaches the agent these commands and conventions. Its `COLLABORATION.md` covers working with other agents.

## Install

Clone the repo and run the installer:

```bash
git clone https://github.com/bdpdx/slack-mcp-server.git
cd slack-mcp-server
./install.sh
```

`install.sh`:

1. **Prerequisites.** It checks for Apple's Command Line Tools and starts their installer if they're missing. It also checks for the Go version `go.mod` asks for, and offers to install or upgrade Go with Homebrew.
2. **Build.** It runs `make build`, which produces `build/slack-mcp-server`.
3. **Link.** It asks where to link the binary. The default is the path from your last install, or `~/.local/bin/slack-mcp-server`. The answer must be a path, and an existing file that isn't a symlink is never replaced.
4. **Setup.** It runs the interactive `slack-mcp-server setup`. Setup finds `~/.claude`, `~/.codex` and any homes saved from earlier runs, and lets you add, remove or re-path homes before it starts. Then for each home:
   - **Bot name.** It asks for a bot name (see [Naming rules](#naming-rules)).
   - **Manifest.** It writes a Slack app manifest to `.install/manifests/<bot>.json` and offers to copy it to the clipboard. It tells you how to create a new app from the manifest, or update an existing app, and how to install it to the workspace.
   - **Tokens.** It asks for the app-level (`xapp-`), bot (`xoxb-`) and user (`xoxp-`) tokens, says where to find each, and checks them with Slack.
   - **Env file.** It writes `<home>/slack-mcp-server.env` (mode 0600) with the tokens and permissive tool defaults. It doesn't ask about tools; edit the file to change them.
   - **Agent integration.** It installs the skill and hooks and registers the `slack` MCP server with the host. For Codex, it also:
     - allows `slack-mcp-server chat` commands in `rules/default.rules`;
     - lets Slack MCP tools run without approval (`default_tools_approval_mode = "approve"`);
     - offers to remove a `codex-push.py` notify, which turn-end DMs replace.
   - **Codex app-server.** Each Codex home also needs a running app-server. Setup creates, enables and starts a launchd agent, `~/Library/LaunchAgents/com.openai.<home dir without the dot>.app-server.plist`. If one already exists, it checks it and offers to start it if it isn't running.
   - **Start script.** Each Codex home also gets a start script beside the binary link (`start-codex`, `start-codex-work`, …):

     ```bash
     start-codex-work [-m|--model <model>] [-p|--project-root <dir>] [codex args...]
     ```

     It runs `codex --remote unix://` against the home's app-server. `-m daybreak` is short for `gpt-daybreak-blue-latest`. The project directory is `-p`, else `PROJECT_ROOT` set in the script, else the current directory. Re-running the installer keeps any `PROJECT_ROOT` you set.

A home that already has an env file offers **Update** (keep tokens, refresh everything else), **Reinstall** (new bot and tokens) or **Skip**. Re-running `./install.sh` is safe:

- Every file it changes is backed up first (`<file>.bak-<timestamp>`), and files are written atomically.
- A config file that isn't valid JSON is left alone.
- Hooks and rules from an earlier install are replaced, not duplicated.

Setup saves the binary path and homes in `.install-state.json`, which is gitignored. A summary at the end lists every change. Restart running Claude Code and Codex sessions after a first install. Codex asks you to trust the new hooks in its next session.

### Upgrading

To upgrade, pull and run `./install.sh` again. At the end it restarts each home's running listener on the new binary (`slack-mcp-server chat --env-file <home>/slack-mcp-server.env listener restart --if-running`); a listener already on that build is left alone (`--force` restarts it anyway). Every session keeps its watches, cohort registrations and delivery marks, and messages posted during the restart are caught up. Hooks and chat commands use the new binary from their next run. A listener built before this command existed, or one that doesn't answer, is stopped with SIGTERM and replaced the same way. Restart succeeds only when the home's listener then reports the new build; otherwise setup lists the failure under **Listeners**, prints the command that starts it, and exits non-zero.

A restart keeps what the state file holds. Approval answers live only in the listener's memory, so before stopping it waits up to 5 seconds for the waiting hooks to collect the ones already given, and refuses to stop (the restart fails; run it again shortly) if any are still uncollected, or while you are still clicking request buttons (each click holds it for 2 seconds). A click or reply that arrives during the restart itself is not recorded: the request's thread gets a note asking you to answer again, and the request stays live. The listener also finishes every delivery already under way (messages, a new watch's backlog, cohort notices) and marks it delivered before it exits, and starts none after the restart begins: those are left to the new listener (a new watch's requested backlog is kept in the state file for it), so nothing arrives twice or goes missing. Cohort mentions waiting to be re-checked are kept in the state file. Catch-up always sends messages oldest first; a long backlog arrives in consecutive parts of up to 50, and each part tells the agent to read every part before acting, since a later message may change or cancel an earlier one. A listener stopped with SIGTERM (one built before this command existed, or one that doesn't answer) gets none of these waits.

Running sessions don't need a restart, with one exception: each session's MCP server (the `conversations_*` and other Slack tools) is a process Claude Code or Codex started, and it stays on the old binary until that session restarts, or until `/mcp` reconnects it in Claude Code. Restart sessions only when a release changes those tools, or when setup changed a home's hooks or MCP configuration; the setup summary names any home that needs it.

`slack-mcp-server chat listener stop` stops a home's listener without starting another.

## Uninstall

```bash
./uninstall.sh
```

`uninstall.sh` rebuilds the binary (so an older build never runs) and starts the interactive `slack-mcp-server uninstall`. It lists the homes it finds and asks about each one separately. The answer defaults to **no**, so you must say yes for each home explicitly.

For each home you confirm, it removes:

- the SlackAgentChat hooks;
- the skill, after backing it up;
- the MCP registration;
- for Codex, the `chat` rule and the `approve` setting;
- `slack-mcp-server.env`, without asking.

If a home's folder no longer exists, it skips the files and still cleans up what lives outside the folder.

For Codex homes it also asks, defaulting to **yes**, whether to:

- remove the app-server launch agent (other processes might be using it);
- delete the home's start script.

Backups of the launch agent and start script go to `.install/backups`.

When no installed home is left, it offers to remove the binary link. The summary reminds you to delete the Slack apps you no longer need at https://api.slack.com/apps.

## Using it

- **Start a project:** ask an agent to "start a Slack chat for project X with codex and claude". The agent invites only the agents and people you name; you are always invited.
- **Join a project:** ask an agent to join `#X`. It runs `chat watch start --channel X`, optionally with `--backlog N` to read recent history.
- **Talk to everyone** in `#X`, or to one agent in `#X__<you>_<agent>`.
- **From any session's prompt:** `%agents: …` posts to that session's project channel as you.
- **Check delivery:** `slack-mcp-server chat watch status`. Listener state and logs are in `<home>/slack-agent-chat/`.

`slack-mcp-server chat --help` lists every command.

## MCP tools

Tools that only read are always on. Tools that change something are each gated by a `SLACK_MCP_*_TOOL` setting in the env file. Most take `true` or a comma-separated allow-list or deny-list of channels.

| Tool | Installer default |
|---|---|
| `conversations_history`, `conversations_replies`, `conversations_search_messages` (user token), `conversations_unreads` (user token), `channels_list`, `channels_me`, `users_search`, `usergroups_list`, `usergroups_me` (list) | always on |
| `conversations_add_message` | on |
| `conversations_join`, `conversations_leave` | on |
| `conversations_rename`, `conversations_set_topic`, `conversations_invite` | on |
| `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `usergroups_me` join/leave | on |
| `attachment_get_data` | on |
| `files_upload` | on |
| `reactions_add`, `reactions_remove`, `conversations_open`, `conversations_create`, `conversations_mark` | off |
| `conversations_delete_message`, `conversations_invite_shared` | off |

File transfers go only through `~/Downloads/slack-mcp` (`SLACK_MCP_FILES_DIR`). Downloads and uploads are limited to 64 MiB, and inline content to 1 MiB.

The server also provides two resources, `slack://<workspace>/channels` and `slack://<workspace>/users`, which are CSV directories of channels and users.

See [Configuration and Usage](docs/03-configuration-and-usage.md) for every setting and tool parameter.

## Configuration

Each home's settings live only in `<home>/slack-mcp-server.env`; environment variables aren't read. The server refuses the file if other users can read it. Without `--env-file`, the server uses the env file of the detected session's home (`${CODEX_HOME:-~/.codex}` or `~/.claude`) and exits if that file is missing.

Only Slack app tokens are supported (`xoxb`, `xoxp`, and `xapp` for Socket Mode); browser session tokens are not. Stdio is the normal transport. SSE and HTTP listen on 127.0.0.1 and need `SLACK_MCP_API_KEY`.

More detail:

- [Authentication Setup](docs/01-authentication-setup.md)
- [Installation](docs/02-installation.md)
- [Configuration and Usage](docs/03-configuration-and-usage.md)
- [Slack Agent Chat](docs/04-slack-agent-chat.md)

## Development

```bash
make build   # build/slack-mcp-server
make test    # go test -race ./...
npx @modelcontextprotocol/inspector ./build/slack-mcp-server --transport stdio --env-file ~/.claude/slack-mcp-server.env
```

## License

MIT; see [LICENSE](LICENSE). Originally written by Dmitrii Korotovskii ([korotovsky/slack-mcp-server](https://github.com/korotovsky/slack-mcp-server)). This is not an official Slack product.
