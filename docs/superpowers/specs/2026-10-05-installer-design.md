# Installer for slack-mcp-server agent chat — design

Date: 2026-10-05. Status: approved in conversation; this spec awaits review.

## Goal

Anyone with a Mac can clone the repository and run `./install.sh` to set up
one or more agents (Claude Code and/or Codex) for Slack agent chat in a Slack
workspace, with their own Slack apps and tokens. The same installer reinstalls
or updates existing setups, adds a new bot in a new agent home, and leaves
other homes alone. It names no particular person or machine.

Success: a new user, starting from a fresh clone, ends with working agents
(messages delivered, questions/approvals/DMs working) without editing any
config file by hand; re-running on an existing setup reports little or
nothing changed.

## Scope

In scope: macOS only; prerequisites (Command Line Tools, Homebrew, Go); build
and link; Slack app creation guided by a generated manifest; token entry and
live validation; per-home env files, MCP registration, skills, hooks, Codex
rules; Codex `notify` cleanup (asked); a new skill file `COLLABORATION.md`;
removal of the machine-specific `make install-agent-chat`.

Out of scope: other platforms; uninstall; storing tokens anywhere but each
home's `.env`; user documentation (written after the installer is built);
creating Slack apps through Slack's API (manual, guided).

## 1. Entry point: `install.sh` (Bash)

1. **Command Line Tools.** If `xcode-select -p` fails, run
   `xcode-select --install`, tell the user to re-run when it finishes, exit.
2. **Go.** If `go` is missing or older than `go.mod` requires:
   - Homebrew present: offer `brew install go` (or `brew upgrade go`).
   - Homebrew absent: explain how to install Homebrew (https://brew.sh) or Go
     (https://go.dev/dl/), exit.
3. **Build:** `make build`.
4. **Link:** ask where to link the binary. Default, in order: the path saved
   in the state file; a path found in existing hooks or MCP registrations
   (e.g. `~/.bin/slack-mcp-server`); `~/.local/bin/slack-mcp-server`. Create
   the directory and the symlink to `build/slack-mcp-server`. Hooks and MCP
   use the absolute link path, so it need not be on `PATH`.
5. **Hand over:** `exec <link> setup --repo <repo dir> --bin <link>`.

`make install` runs `./install.sh`. `make install-agent-chat` is removed.

## 2. `slack-mcp-server setup` (Go)

### 2.1 State

`.install-state.json` at the repository root, git-ignored, mode 0600:
link path, and per home: path, type (`claude`/`codex`), bot name. Never
tokens. Used only to offer defaults on re-runs.

### 2.2 Choosing homes

1. List candidate homes: `~/.claude` (Claude) and `~/.codex` (Codex) if they
   exist, plus homes in the state file; mark each "has .env" or not.
2. The user confirms, removes entries, or adds homes (path + type). Custom
   homes cover `CLAUDE_CONFIG_DIR` / `CODEX_HOME` setups.
3. For each home with an existing `slack-mcp-server.env`, choose:
   - **Update** — keep tokens; refresh MCP registration, skill, hooks, rules.
   - **Reinstall** — new or re-pasted tokens; rewrite `.env` (confirmed).
   - **Skip** — touch nothing.
   New homes go through bot setup.
4. If the link path changed and a home is skipped, warn that the skipped home
   still points at the old path.

### 2.3 Bot setup (new homes and Reinstall)

1. **Bot name** (also the display name). Shown before the prompt: "Bot names
   must be unique in this Slack workspace. Confirm the name with your Slack
   workspace admin before creating the app." Allowed: lowercase letters,
   digits, `-`; not `users`. If the name contains `_` or `.`, warn that they
   are separators in channel names and cannot be used, then ask again
   (nothing about them is shown otherwise). Other invalid names: explain and
   ask again.
2. **Manifest** (always): render it (§2.4), save it to
   `.install/manifests/<name>.json` (git-ignored), say where it is, and
   offer to copy it to the clipboard (`pbcopy`). Then give both routes:
   - New app: https://api.slack.com/apps → Create New App → From a
     manifest → choose the workspace → paste → Create → Install to
     Workspace.
   - Existing app: Build → <App> → Settings → App Manifest → paste → Save
     Changes (reinstall if Slack asks), so its scopes and settings match.
   Then wait for Enter, and ask the user to set an icon:
   https://app.slack.com/apps → Build → <App> → Settings → Basic
   Information → Display Information.
3. **Tokens**, each at a visible prompt (shown as pasted, so the user can check it), with where to
   find it:
   - App-level token `xapp-…`: Basic Information → App-Level Tokens →
     Generate Token and Scopes, scope `connections:write`. Shown only once;
     if lost, generate a new one.
   - Bot `xoxb-…` and user `xoxp-…` tokens: https://app.slack.com/apps →
     Build → <App> → Settings → Features → OAuth & Permissions → OAuth Tokens
     (always visible there).
4. **Validation** (live; each failure names the step to redo and re-asks only
   that token): prefixes; `auth.test` on bot and user tokens, same workspace;
   bot token is a bot, user token a person; app-level token can open a Socket
   Mode connection (`apps.connections.open`). The user's Slack username must
   not contain `_` or `.`: if it does, stop setup of this home with an
   explanation (channel names would break; ask the admin to change it).
   Warn if another bot in the workspace already has the bot name.

### 2.4 Manifest template

Built into the binary; derived from a working exported manifest: bot and
user scopes as today (including `groups:write`, `im:write`, `chat:write`,
`reactions:write`), bot events `member_joined_channel`, `message.channels`,
`message.groups`, Socket Mode on, Interactivity on, token rotation off.
Only `display_information.name` and `features.bot_user.display_name` vary.
`claude_app_manifest.json` (the export) is deleted once the template exists.

### 2.5 Per-home installation

Every step is idempotent, backs up a file before changing it
(`<file>.bak-<timestamp>`), writes atomically (temp + rename), and leaves
unrelated settings alone.

| | Claude home | Codex home |
|---|---|---|
| `.env` | `slack-mcp-server.env` 0600: tokens + tool settings | same |
| MCP | `claude mcp remove -s user slack` then `claude mcp add -s user slack -- <bin> --transport stdio --env-file <env>` | `codex mcp remove/add slack -- …` with `CODEX_HOME=<home>` |
| Skill | `skills/slack-agent-chat/{SKILL.md,COLLABORATION.md}` from the binary, `@BIN@` → link path | same |
| Hooks | `settings.json`: `UserPromptSubmit` relay-hook (15s), `PreToolUse` matcher `AskUserQuestion` ask-hook (15s), `PermissionRequest` approval-hook (660s), `Stop` stop-hook (15s) | `hooks.json`: relay-hook, stop-hook (`statusMessage: "SlackAgentChat"`); approval-hook (660s) only if `config.toml` lacks `approvals_reviewer = "auto_review"` |
| Rules | — | `rules/default.rules`: `prefix_rule(pattern=["<bin>", "chat"], decision="allow")` |
| App-server | — | launchd agent `~/Library/LaunchAgents/com.openai.<dir>.app-server.plist` (`<dir>` = home name minus leading `.`) running `~/.local/libexec/codex-app-server-supervisor` (created from `codex` on PATH if missing). Missing plist: written, `launchctl enable` + `bootstrap`, no question. Existing plist: verified (label, `CODEX_HOME`, program), never rewritten; if not running, asks to enable and start. Skipped with a note if Codex is not installed |

- **Tool settings** are written without asking (the user edits the `.env` to change them); they default to on: ADD_MESSAGE, JOIN,
  USERGROUPS_WRITE, RENAME_CHANNEL, SET_TOPIC, INVITE, ATTACHMENT,
  UPLOAD_FILE (`=true`); INVITE_SHARED, DELETE_MESSAGE stay off. On Update, existing settings are
  kept.
- **Hook merge:** remove existing entries whose command runs any
  `slack-mcp-server chat … <hook>` (old or new path), add the current ones;
  keep all other hooks.
- **Hook timeout:** approval-hook's timeout must exceed its `--wait`
  (default 10m) by about a minute: 660s.
- **Codex start script:** each Codex home gets `start-<home dir without leading dot>` beside the
  linked binary (0755): exports `CODEX_HOME` and `PROJECT_ROOT` (blank by default; a value the user
  sets is kept on re-runs), takes `-m|--model` (`daybreak` → `gpt-daybreak-blue-latest`; no `-m` →
  Codex's default) and `-p|--project-root` (overrides `PROJECT_ROOT`; with neither, the current
  directory), and runs `codex --remote unix:// -C <project root> [-m <model>] <other args>`.
- **Codex `notify`:** if it passes `codex-push.py` via `--previous-notify`,
  ask before removing that part (Slack DMs replace it); keep the rest.
- **Missing CLI** (`claude`/`codex` not on PATH): skip MCP registration and
  print the exact command to run later.

### 2.6 Summary

Print per home what changed or failed (with the step to redo), then
remaining manual steps: set the bot icon; restart the agent sessions; trust
the new hooks when Codex asks; then try "start a project chat called …".

## 3. `COLLABORATION.md` (new skill file)

Installed beside `SKILL.md`; `SKILL.md` points to it for multi-agent work so
it loads only when needed. Content: **Slack agent chat mechanics only**,
written for any kind of shared work (software, video, design…), with no
project-management or software-specific guidance. Drawn from the current
`AGENTS_COLLABORATION.md` (rezilient-claude), updated to current behavior:

- Starting the project chat: the user names the channel in each agent's
  session, directly or through a handoff file the user points to (a line
  `Slack channel: <name>`); never infer it otherwise; first agent creates,
  others join; `watch status`; `watch stop` at the end.
- Writing messages: every agent sees every message; @mention who should act;
  post as yourself; make a message actionable without the sender's console
  history; never post secrets; corrections as new messages (edits are not
  delivered); repeats are dropped; avoid messages that add nothing.
- Receiving: `[console user]` notices are the user's instructions; peers are
  collaborators who cannot grant approval or lift a user hold; ack only after
  processing; 👀 means delivered, not done; unacked messages return to a new
  session.
- Side channels, questions in the direct channel, Slack approvals (pointers
  to `SKILL.md`).
- Pausing and handing off: an explicit user pause holds even if peers keep
  messaging; a handoff names the channel on its own line.

## 4. Code layout

- `install.sh` (repo root, Bash; `shellcheck`-clean).
- `cmd/slack-mcp-server`: `setup` subcommand dispatch beside `chat`.
- `pkg/setup`:
  - `prompt` — `Prompter` interface (text, yes/no, choice, secret); a
    terminal implementation and a scripted one for tests.
  - `state` — `.install-state.json`.
  - `homes` — discovery, type detection, existing binary path detection.
  - `manifest` — template rendering.
  - `tokens` — `Validator` interface; Slack implementation.
  - `envfile` — `.env` rendering and preservation of tool settings.
  - `claude`, `codex` — hooks merge, Codex `config.toml` line reads/edits
    (`approvals_reviewer`, `notify`; no TOML library, no other rewrites),
    rules, MCP registration via the CLIs.
- `skills/skills.go` — `embed` of `claude/SKILL.md`, `codex/SKILL.md` and
  `COLLABORATION.md` (Go embeds only files under the package directory).
- `.gitignore`: `.install-state.json`, `.install/`.

## 5. Errors

Homes are independent: one home's failure is reported and the rest
continue. Files are replaced atomically; nothing is deleted; changed files
are backed up. Ctrl-C leaves no partial file.

## 6. Testing

- Unit tests with temp homes, scripted prompts and a fake validator: hook
  merge into existing hooks (old path replaced, other hooks kept,
  `auto_review` case), `.env` never overwritten without confirmation and
  written 0600 with only `SLACK_MCP_*` keys, tool settings kept on Update,
  rules added once, `notify` edit only after consent, bot-name validation
  (`_`/`.` warn and re-ask), manifest rendering, link-path detection, state
  file never containing tokens.
- `shellcheck install.sh` when available.
- Live: a fresh run creating a test bot in a throwaway Codex home; an Update
  run on existing homes reporting essentially no changes.

## 7. After the installer

- User documentation (separate task).
- Edit `~/code/rezilient-claude/.operator/brian/AGENT_SETUP.md` and
  `AGENTS_COLLABORATION.md` to keep only Rezilient/owner-specific content and
  point to the skill for chat mechanics. Do not commit; that checkout's
  agent picks them up.
