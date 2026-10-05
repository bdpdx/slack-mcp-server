# Slack Agent Chat Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Push Slack channel messages into running Codex and Claude Code sessions, so Slack replaces the filesystem `agent-chat` mailbox.

**Architecture:** A new `pkg/agentchat` package holds every piece: env-file loading, routing, notices, persisted state, Codex and Claude delivery, a local control socket, the per-home listener, and the `slack-mcp-server chat …` CLI. The MCP server only gains `--env-file`. Skills and a `%agents:` hook live in `skills/slack-agent-chat/` and are installed by a Makefile target.

**Tech Stack:** Go 1.25, `github.com/slack-go/slack` v0.19.0 (incl. `socketmode`), `github.com/gorilla/websocket` v1.5.3, `github.com/joho/godotenv` v1.5.1, `github.com/google/uuid`, `go.uber.org/zap`, `testify`.

**Spec:** `docs/superpowers/specs/2026-10-04-slack-agent-chat-design.md`

## Global Constraints

- Env file name: `slack-mcp-server.env`; flag `--env-file`; new key `SLACK_MCP_XAPP_TOKEN`.
- Inherited `SLACK_MCP_*` variables are cleared before loading the file; a missing file is a fatal error (exit 1, message on stderr).
- State dir `<home>/slack-agent-chat/` mode 0700; files inside 0600; control socket `listener.sock`.
- Listener exits 60 s after its last subscription ends.
- Repeat window 10 minutes, agent senders only.
- Notice text truncated at 4000 runes; recovery batch capped at 50 messages.
- Reactions: delivered `eyes`; acknowledged `white_check_mark`.
- Codex app-server JSON-RPC messages carry no `"jsonrpc"` field.
- Unix socket paths in tests must be short (macOS limit ~104 bytes): create them under `os.MkdirTemp("/tmp", "sac")`.
- Module path: `github.com/korotovsky/slack-mcp-server`.

## Review Focus

- Agent replies write plain `@codex-r`, not `<@U…>`: routing must treat plain `@name` of a bot member as a mention (Task 2 test `TestAgentMentionsPlainName`).
- A Claude session that ended leaves a dead socket path: delivery must drop that subscription, not retry forever (Task 7 test `TestListenerDropsGoneClaudeSession`).
- The turn ends between `thread/read` and `turn/steer`: the steer is rejected and delivery must re-read and start a new turn (Task 5 test `TestDeliverCodexRetriesAfterSteerRejected`).
- A `%agents:` relay comes back over Slack to the session that sent it: it must not be delivered there (Task 7 test `TestListenerSkipSuppressesDelivery`).
- Slack redelivers an event (retry): the same message must not be pushed twice to a session (Task 7 test `TestListenerDoesNotRedeliver`).

---

## File Structure

| File | Responsibility |
|---|---|
| `pkg/agentchat/envfile.go` | Resolve and load the env file; `Home` paths |
| `pkg/agentchat/route.go` | `Message`, `Identity`, mention parsing, routing rule, repeat filter |
| `pkg/agentchat/notice.go` | Notice text for one message or a batch |
| `pkg/agentchat/state.go` | Persisted subscriptions, join points, delivery ledger; ts helpers |
| `pkg/agentchat/claude.go` | Claude inbox-socket delivery |
| `pkg/agentchat/codex.go` | Codex app-server client, delivery, `codex queue` fallback |
| `pkg/agentchat/deliver.go` | `Deliverer` interface and `HostDeliverer` |
| `pkg/agentchat/control.go` | Control-socket protocol (server + client) |
| `pkg/agentchat/listener.go` | Listener: directory, routing, delivery, recovery |
| `pkg/agentchat/daemon.go` | Socket Mode wiring, `RunListener`, payload parsing |
| `pkg/agentchat/cli.go` | `chat` subcommands |
| `cmd/slack-mcp-server/main.go` | `--env-file` and `chat` dispatch |
| `skills/slack-agent-chat/codex/SKILL.md`, `hooks.json` | Codex skill and hook |
| `skills/slack-agent-chat/claude/SKILL.md` | Claude skill |
| `Makefile` | `install-agent-chat` target |
| `docs/04-slack-agent-chat.md` | Setup and usage |

---

### Task 1: Env file loading and the `--env-file` flag

**Files:**
- Create: `pkg/agentchat/envfile.go`
- Test: `pkg/agentchat/envfile_test.go`
- Modify: `cmd/slack-mcp-server/main.go:23-36`, `go.mod`

**Interfaces:**
- Produces: `const EnvFileName = "slack-mcp-server.env"`; `ResolveEnvFile(flagValue string, getenv func(string) string) (string, error)`; `ExpandHome(path, home string) string`; `LoadEnvFile(path string) error`; `type Home struct{ Dir, EnvFile, StateDir, ControlSocket, StateFile, LogFile, CodexSocket string }`; `NewHome(envFile string) Home`.

- [ ] **Step 1: Write the failing test**

```go
package agentchat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveEnvFile(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  map[string]string
		want string
	}{
		{"flag wins", "~/x/f.env", map[string]string{"HOME": "/h", "CODEX_THREAD_ID": "t"}, "/h/x/f.env"},
		{"codex default home", "", map[string]string{"HOME": "/h", "CODEX_THREAD_ID": "t"}, "/h/.codex/slack-mcp-server.env"},
		{"codex explicit home", "", map[string]string{"HOME": "/h", "CODEX_THREAD_ID": "t", "CODEX_HOME": "~/.codex-rezilient"}, "/h/.codex-rezilient/slack-mcp-server.env"},
		{"claude default", "", map[string]string{"HOME": "/h", "CLAUDECODE": "1"}, "/h/.claude/slack-mcp-server.env"},
		{"claude config dir", "", map[string]string{"HOME": "/h", "CLAUDECODE": "1", "CLAUDE_CONFIG_DIR": "/c"}, "/c/slack-mcp-server.env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveEnvFile(c.flag, envMap(c.env))
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
	_, err := ResolveEnvFile("", envMap(map[string]string{"HOME": "/h"}))
	assert.Error(t, err)
}

func TestLoadEnvFileReplacesInheritedSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, EnvFileName)
	require.NoError(t, os.WriteFile(path, []byte("SLACK_MCP_XOXB_TOKEN=xoxb-file\nSLACK_MCP_ADD_MESSAGE_TOOL=true\n"), 0o600))
	t.Setenv("SLACK_MCP_XOXB_TOKEN", "xoxb-shell")
	t.Setenv("SLACK_MCP_XOXP_TOKEN", "xoxp-shell")

	require.NoError(t, LoadEnvFile(path))

	assert.Equal(t, "xoxb-file", os.Getenv("SLACK_MCP_XOXB_TOKEN"))
	assert.Equal(t, "true", os.Getenv("SLACK_MCP_ADD_MESSAGE_TOOL"))
	_, inherited := os.LookupEnv("SLACK_MCP_XOXP_TOKEN")
	assert.False(t, inherited, "inherited SLACK_MCP_* must be cleared")
}

func TestLoadEnvFileMissing(t *testing.T) {
	err := LoadEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	assert.ErrorContains(t, err, "nope.env")
}

func TestNewHome(t *testing.T) {
	h := NewHome("/h/.codex/slack-mcp-server.env")
	assert.Equal(t, "/h/.codex", h.Dir)
	assert.Equal(t, "/h/.codex/slack-agent-chat", h.StateDir)
	assert.Equal(t, "/h/.codex/slack-agent-chat/listener.sock", h.ControlSocket)
	assert.Equal(t, "/h/.codex/slack-agent-chat/state.json", h.StateFile)
	assert.Equal(t, "/h/.codex/slack-agent-chat/listener.log", h.LogFile)
	assert.Equal(t, "/h/.codex/app-server-control/app-server-control.sock", h.CodexSocket)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run 'TestResolveEnvFile|TestLoadEnvFile|TestNewHome' -v`
Expected: FAIL — `undefined: ResolveEnvFile` (package does not compile).

- [ ] **Step 3: Write minimal implementation**

`pkg/agentchat/envfile.go`:

```go
// Package agentchat delivers Slack channel messages into running Codex and
// Claude Code sessions and provides the `slack-mcp-server chat` helpers.
package agentchat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

// EnvFileName is the configuration file kept in each agent's home directory.
const EnvFileName = "slack-mcp-server.env"

// ResolveEnvFile returns the env file to load: flagValue when given,
// otherwise the file in the detected host's home (Codex when CODEX_THREAD_ID
// is set, Claude Code when CLAUDECODE is set).
func ResolveEnvFile(flagValue string, getenv func(string) string) (string, error) {
	home := getenv("HOME")
	if flagValue != "" {
		return ExpandHome(flagValue, home), nil
	}
	if getenv("CODEX_THREAD_ID") != "" {
		dir := getenv("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		return filepath.Join(ExpandHome(dir, home), EnvFileName), nil
	}
	if getenv("CLAUDECODE") != "" {
		dir := getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			dir = filepath.Join(home, ".claude")
		}
		return filepath.Join(ExpandHome(dir, home), EnvFileName), nil
	}
	return "", fmt.Errorf("no --env-file given and no Codex or Claude Code session detected")
}

// ExpandHome replaces a leading "~" with home.
func ExpandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// LoadEnvFile makes path the only source of SLACK_MCP_* settings: inherited
// SLACK_MCP_* variables are unset, then the file's values are set.
func LoadEnvFile(path string) error {
	values, err := godotenv.Read(path)
	if err != nil {
		return fmt.Errorf("reading env file %s: %w", path, err)
	}
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "SLACK_MCP_") {
			if err := os.Unsetenv(name); err != nil {
				return err
			}
		}
	}
	for k, v := range values {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// Home is one agent's home directory (for example ~/.codex) and the
// slack-agent-chat files kept under it.
type Home struct {
	Dir           string
	EnvFile       string
	StateDir      string
	ControlSocket string
	StateFile     string
	LogFile       string
	CodexSocket   string
}

// NewHome derives a Home from the env file inside it.
func NewHome(envFile string) Home {
	dir := filepath.Dir(envFile)
	state := filepath.Join(dir, "slack-agent-chat")
	return Home{
		Dir:           dir,
		EnvFile:       envFile,
		StateDir:      state,
		ControlSocket: filepath.Join(state, "listener.sock"),
		StateFile:     filepath.Join(state, "state.json"),
		LogFile:       filepath.Join(state, "listener.log"),
		CodexSocket:   filepath.Join(dir, "app-server-control", "app-server-control.sock"),
	}
}
```

- [ ] **Step 4: Promote dependencies and run tests**

Run: `go get github.com/joho/godotenv@v1.5.1 && go test ./pkg/agentchat/ -run 'TestResolveEnvFile|TestLoadEnvFile|TestNewHome' -v`
Expected: PASS. `go.mod` now lists `github.com/joho/godotenv` in the direct `require` block.

- [ ] **Step 5: Wire `--env-file` into the MCP server**

In `cmd/slack-mcp-server/main.go`, add the import `"github.com/korotovsky/slack-mcp-server/pkg/agentchat"`, declare `var envFile string` with the other flag variables, register the flag before `flag.Parse()`, and load the file immediately after it (before `SLACK_MCP_ENABLED_TOOLS` is read):

```go
	flag.StringVar(&envFile, "env-file", "", "Path to the slack-mcp-server.env file (default: detected from the Codex or Claude Code session)")
	flag.Parse()

	envPath, err := agentchat.ResolveEnvFile(envFile, os.Getenv)
	if err == nil {
		err = agentchat.LoadEnvFile(envPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "slack-mcp-server: %v\n", err)
		os.Exit(1)
	}
```

The existing `logger, err := newLogger(transport)` line still compiles: `logger` is a new variable, so `:=` reuses `err`.

- [ ] **Step 6: Build and verify the missing-file error**

Run: `go build -o /tmp/sac-mcp ./cmd/slack-mcp-server && /tmp/sac-mcp --env-file /tmp/does-not-exist.env; echo "exit=$?"`
Expected: `slack-mcp-server: reading env file /tmp/does-not-exist.env: open /tmp/does-not-exist.env: no such file or directory` and `exit=1`.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum pkg/agentchat/envfile.go pkg/agentchat/envfile_test.go cmd/slack-mcp-server/main.go
git commit -m "Load configuration only from a per-home env file"
```

---

### Task 2: Routing and the repeat filter

**Files:**
- Create: `pkg/agentchat/route.go`
- Test: `pkg/agentchat/route_test.go`

**Interfaces:**
- Produces: `type Message struct{ Channel, TS, ThreadTS, User, BotID, Text, SubType string; Files []string }`; `(Message) From(Identity) bool`; `(Message) Deliverable() bool`; `type Identity struct{ UserID, BotID string }`; `AgentMentions(text string, isAgent func(string) bool, resolve func(string) string) []string`; `ShouldDeliver(m Message, self Identity, mentions []string) bool`; `type RepeatFilter`; `NewRepeatFilter(window time.Duration, now func() time.Time) *RepeatFilter`; `(*RepeatFilter) Repeat(m Message) bool`; package var `idMention *regexp.Regexp`.

- [ ] **Step 1: Write the failing test**

```go
package agentchat

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var agents = map[string]string{"UCB": "codex-b", "UCR": "codex-r", "UCL": "claude"}

func isAgent(id string) bool { _, ok := agents[id]; return ok }
func resolveAgent(name string) string {
	for id, n := range agents {
		if n == name {
			return id
		}
	}
	return ""
}

func TestAgentMentionsTokens(t *testing.T) {
	got := AgentMentions("<@UCR> and <@UBRIAN> please", isAgent, resolveAgent)
	assert.Equal(t, []string{"UCR"}, got)
}

func TestAgentMentionsPlainName(t *testing.T) {
	got := AgentMentions("@codex-r, can you check? cc @claude.", isAgent, resolveAgent)
	assert.ElementsMatch(t, []string{"UCR", "UCL"}, got)
	assert.Empty(t, AgentMentions("mail me at x@codex-r.com", isAgent, resolveAgent))
	assert.Empty(t, AgentMentions("@brian thoughts?", isAgent, resolveAgent))
}

func TestShouldDeliver(t *testing.T) {
	self := Identity{UserID: "UCL", BotID: "BCL"}
	broadcast := Message{User: "UCB", Text: "hi all"}
	assert.True(t, ShouldDeliver(broadcast, self, nil))
	assert.False(t, ShouldDeliver(Message{User: "UCL", BotID: "BCL"}, self, nil), "own message")
	assert.True(t, ShouldDeliver(broadcast, self, []string{"UCL"}))
	assert.False(t, ShouldDeliver(broadcast, self, []string{"UCR"}))
}

func TestDeliverable(t *testing.T) {
	for _, st := range []string{"", "thread_broadcast", "bot_message", "file_share"} {
		assert.True(t, Message{SubType: st}.Deliverable(), st)
	}
	for _, st := range []string{"message_changed", "message_deleted", "channel_join"} {
		assert.False(t, Message{SubType: st}.Deliverable(), st)
	}
}

func TestRepeatFilter(t *testing.T) {
	now := time.Unix(1000, 0)
	f := NewRepeatFilter(10*time.Minute, func() time.Time { return now })
	m := Message{Channel: "C1", User: "UCB", Text: " same "}
	assert.False(t, f.Repeat(m))
	assert.True(t, f.Repeat(Message{Channel: "C1", User: "UCB", Text: "same"}))
	assert.False(t, f.Repeat(Message{Channel: "C1", ThreadTS: "1.0", User: "UCB", Text: "same"}), "different thread")
	assert.False(t, f.Repeat(Message{Channel: "C1", User: "UCR", Text: "same"}), "different sender")
	now = now.Add(11 * time.Minute)
	assert.False(t, f.Repeat(m), "window expired")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run 'TestAgentMentions|TestShouldDeliver|TestDeliverable|TestRepeatFilter' -v`
Expected: FAIL — `undefined: AgentMentions`.

- [ ] **Step 3: Write minimal implementation**

`pkg/agentchat/route.go`:

```go
package agentchat

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// Message is one Slack channel message as the listener sees it.
type Message struct {
	Channel  string
	TS       string
	ThreadTS string
	User     string
	BotID    string
	Text     string
	SubType  string
	Files    []string
}

// Identity is this home's bot user.
type Identity struct {
	UserID string
	BotID  string
}

// From reports whether id posted m.
func (m Message) From(id Identity) bool {
	return (m.User != "" && m.User == id.UserID) || (m.BotID != "" && m.BotID == id.BotID)
}

var deliverableSubtypes = map[string]bool{"": true, "thread_broadcast": true, "bot_message": true, "file_share": true}

// Deliverable reports whether m carries new content (not an edit, delete or join).
func (m Message) Deliverable() bool { return deliverableSubtypes[m.SubType] }

var (
	idMention   = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	nameMention = regexp.MustCompile(`(?:^|[^\w<@.])@([A-Za-z0-9][A-Za-z0-9._-]*)`)
)

// AgentMentions returns the user IDs of agents that text addresses, from
// Slack mention tokens (<@U…>) and plain "@name" text. resolve maps a plain
// name to a user ID ("" when unknown); isAgent filters to bot users.
func AgentMentions(text string, isAgent func(userID string) bool, resolve func(name string) string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] && isAgent(id) {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, m := range idMention.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range nameMention.FindAllStringSubmatch(text, -1) {
		add(resolve(strings.TrimRight(m[1], "._-")))
	}
	return out
}

// ShouldDeliver applies the routing rule: with no agent mentions a message
// goes to every agent except its sender; with agent mentions, only to them.
func ShouldDeliver(m Message, self Identity, mentions []string) bool {
	if m.From(self) {
		return false
	}
	if len(mentions) == 0 {
		return true
	}
	for _, id := range mentions {
		if id == self.UserID {
			return true
		}
	}
	return false
}

// RepeatFilter detects an agent re-sending the same text to the same place.
type RepeatFilter struct {
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	seen   map[string]time.Time
}

// NewRepeatFilter returns a filter that treats identical messages within window as repeats.
func NewRepeatFilter(window time.Duration, now func() time.Time) *RepeatFilter {
	return &RepeatFilter{window: window, now: now, seen: map[string]time.Time{}}
}

// Repeat records m and reports whether the same sender posted the same
// trimmed text in the same channel and thread within the window.
func (f *RepeatFilter) Repeat(m Message) bool {
	key := strings.Join([]string{m.Channel, m.ThreadTS, m.User, m.BotID, strings.TrimSpace(m.Text)}, "\x00")
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for k, t := range f.seen {
		if now.Sub(t) > f.window {
			delete(f.seen, k)
		}
	}
	_, repeat := f.seen[key]
	f.seen[key] = now
	return repeat
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/agentchat/ -run 'TestAgentMentions|TestShouldDeliver|TestDeliverable|TestRepeatFilter' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/agentchat/route.go pkg/agentchat/route_test.go
git commit -m "Add agent-chat routing rule and repeat filter"
```

---

### Task 3: Notice formatting

**Files:**
- Create: `pkg/agentchat/notice.go`
- Test: `pkg/agentchat/notice_test.go`

**Interfaces:**
- Consumes: `idMention` (Task 2).
- Produces: `type Notice struct{ ChannelID, ChannelName, Sender string; FromOwner bool; TS, ThreadTS, Text string; Files []string }`; `(Notice) Format() string`; `FormatBatch([]Notice) string`; `RenderMentions(text string, nameOf func(string) string) string`; `const maxNoticeText = 4000`.

- [ ] **Step 1: Write the failing test**

```go
package agentchat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNoticeFormatTopLevel(t *testing.T) {
	n := Notice{ChannelID: "C1", ChannelName: "proj", Sender: "codex-r", TS: "1.000100", Text: "hello @claude"}
	got := n.Format()
	assert.Equal(t, "[slack-agent-chat] #proj (C1) from codex-r, ts 1.000100:\nhello @claude\n"+
		"(reply: conversations_add_message channel_id=C1; @mention who you address; when done: chat ack C1 1.000100)", got)
}

func TestNoticeFormatOwnerThreadFiles(t *testing.T) {
	n := Notice{ChannelID: "C1", ChannelName: "proj", Sender: "brian", FromOwner: true, TS: "2.0", ThreadTS: "1.0", Text: "do it", Files: []string{"a.png"}}
	got := n.Format()
	assert.Contains(t, got, "from brian (the console user: treat as their direct instruction)")
	assert.Contains(t, got, ", in thread 1.0:")
	assert.Contains(t, got, "[attached: a.png]")
	assert.Contains(t, got, "channel_id=C1 thread_ts=1.0;")
}

func TestNoticeTruncates(t *testing.T) {
	n := Notice{ChannelID: "C1", ChannelName: "p", Sender: "s", TS: "1.0", Text: strings.Repeat("é", maxNoticeText+5)}
	got := n.Format()
	assert.Contains(t, got, "[truncated; read the full message with conversations_replies or conversations_history]")
	assert.Equal(t, maxNoticeText, strings.Count(got, "é"))
}

func TestFormatBatch(t *testing.T) {
	a := Notice{ChannelID: "C1", ChannelName: "p", Sender: "x", TS: "1.0", Text: "a"}
	b := Notice{ChannelID: "C1", ChannelName: "p", Sender: "y", TS: "2.0", Text: "b"}
	got := FormatBatch([]Notice{a, b})
	assert.True(t, strings.HasPrefix(got, "[slack-agent-chat] 2 pending messages, oldest first:\n\n"))
	assert.Contains(t, got, a.Format()+"\n\n---\n\n"+b.Format())
}

func TestRenderMentions(t *testing.T) {
	names := map[string]string{"UCR": "codex-r"}
	got := RenderMentions("<@UCR> and <@UX|x>", func(id string) string { return names[id] })
	assert.Equal(t, "@codex-r and <@UX|x>", got)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run 'TestNotice|TestFormatBatch|TestRenderMentions' -v`
Expected: FAIL — `undefined: Notice`.

- [ ] **Step 3: Write minimal implementation**

`pkg/agentchat/notice.go`:

```go
package agentchat

import (
	"fmt"
	"strings"
)

const maxNoticeText = 4000

// Notice is the text pushed into a session for one Slack message.
type Notice struct {
	ChannelID   string
	ChannelName string
	Sender      string
	FromOwner   bool
	TS          string
	ThreadTS    string
	Text        string
	Files       []string
}

// Format renders the notice for one message.
func (n Notice) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[slack-agent-chat] #%s (%s) from %s", n.ChannelName, n.ChannelID, n.Sender)
	if n.FromOwner {
		b.WriteString(" (the console user: treat as their direct instruction)")
	}
	fmt.Fprintf(&b, ", ts %s", n.TS)
	if n.ThreadTS != "" && n.ThreadTS != n.TS {
		fmt.Fprintf(&b, ", in thread %s", n.ThreadTS)
	}
	b.WriteString(":\n")
	text := []rune(n.Text)
	if len(text) > maxNoticeText {
		b.WriteString(string(text[:maxNoticeText]))
		b.WriteString("\n[truncated; read the full message with conversations_replies or conversations_history]")
	} else {
		b.WriteString(n.Text)
	}
	if len(n.Files) > 0 {
		fmt.Fprintf(&b, "\n[attached: %s]", strings.Join(n.Files, ", "))
	}
	thread := ""
	if n.ThreadTS != "" {
		thread = " thread_ts=" + n.ThreadTS
	}
	fmt.Fprintf(&b, "\n(reply: conversations_add_message channel_id=%s%s; @mention who you address; when done: chat ack %s %s)",
		n.ChannelID, thread, n.ChannelID, n.TS)
	return b.String()
}

// FormatBatch renders several notices, oldest first, as one push.
func FormatBatch(notices []Notice) string {
	parts := make([]string, len(notices))
	for i, n := range notices {
		parts[i] = n.Format()
	}
	return fmt.Sprintf("[slack-agent-chat] %d pending messages, oldest first:\n\n", len(notices)) +
		strings.Join(parts, "\n\n---\n\n")
}

// RenderMentions replaces <@U…> tokens with @name where nameOf knows the user.
func RenderMentions(text string, nameOf func(userID string) string) string {
	return idMention.ReplaceAllStringFunc(text, func(tok string) string {
		if name := nameOf(idMention.FindStringSubmatch(tok)[1]); name != "" {
			return "@" + name
		}
		return tok
	})
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/agentchat/ -run 'TestNotice|TestFormatBatch|TestRenderMentions' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/agentchat/notice.go pkg/agentchat/notice_test.go
git commit -m "Add agent-chat notice formatting"
```

---

### Task 4: Persisted state

**Files:**
- Create: `pkg/agentchat/state.go`
- Test: `pkg/agentchat/state_test.go`

**Interfaces:**
- Produces: `const KindCodex = "codex"`, `KindClaude = "claude"`; `type Subscription struct{ SessionID, Kind, ThreadID, Socket, Token string; Channels []string }` (JSON tags `session_id, kind, thread_id, socket, token, channels`); `(*Subscription) Watches(channel string) bool`; `type State struct{ JoinTS map[string]string; Subscriptions map[string]*Subscription; Delivered map[string]int64 }`; `LoadState(path string) (*State, error)`; `(*State) Save(path string) error`; `(*State) WasDelivered(session, channel, ts string) bool`; `(*State) MarkDelivered(session, channel, ts string, now time.Time)`; `(*State) Prune(now time.Time)`; `(*State) Watchers(channel string) []*Subscription`; `NowTS(time.Time) string`; `TSLess(a, b string) bool`.

- [ ] **Step 1: Write the failing test**

```go
package agentchat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := LoadState(path)
	require.NoError(t, err)
	assert.Empty(t, s.Subscriptions)

	s.JoinTS["C1"] = "100.000000"
	s.Subscriptions["sess"] = &Subscription{SessionID: "sess", Kind: KindClaude, Socket: "/s", Token: "tok", Channels: []string{"C1"}}
	s.MarkDelivered("sess", "C1", "101.0", time.Unix(5000, 0))
	require.NoError(t, s.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	got, err := LoadState(path)
	require.NoError(t, err)
	assert.Equal(t, "100.000000", got.JoinTS["C1"])
	assert.Equal(t, "tok", got.Subscriptions["sess"].Token)
	assert.True(t, got.WasDelivered("sess", "C1", "101.0"))
	assert.False(t, got.WasDelivered("other", "C1", "101.0"))
}

func TestStatePrune(t *testing.T) {
	s := &State{JoinTS: map[string]string{}, Subscriptions: map[string]*Subscription{}, Delivered: map[string]int64{}}
	s.MarkDelivered("a", "C", "1.0", time.Unix(0, 0))
	s.MarkDelivered("a", "C", "2.0", time.Unix(8*24*3600, 0))
	s.Prune(time.Unix(8*24*3600, 0))
	assert.False(t, s.WasDelivered("a", "C", "1.0"))
	assert.True(t, s.WasDelivered("a", "C", "2.0"))
}

func TestWatchers(t *testing.T) {
	s := &State{Subscriptions: map[string]*Subscription{
		"b": {SessionID: "b", Channels: []string{"C1"}},
		"a": {SessionID: "a", Channels: []string{"C1", "C2"}},
		"c": {SessionID: "c", Channels: []string{"C2"}},
	}}
	ws := s.Watchers("C1")
	require.Len(t, ws, 2)
	assert.Equal(t, "a", ws[0].SessionID)
	assert.Equal(t, "b", ws[1].SessionID)
}

func TestTS(t *testing.T) {
	assert.Equal(t, "1759600000.000123", NowTS(time.Unix(1759600000, 123456)))
	assert.True(t, TSLess("9.000001", "10.000000"))
	assert.True(t, TSLess("10.000001", "10.000002"))
	assert.False(t, TSLess("10.000002", "10.000002"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run 'TestState|TestWatchers|TestTS' -v`
Expected: FAIL — `undefined: LoadState`.

- [ ] **Step 3: Write minimal implementation**

`pkg/agentchat/state.go`:

```go
package agentchat

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	KindCodex  = "codex"
	KindClaude = "claude"

	deliveredRetention = 7 * 24 * time.Hour
)

// Subscription is one session watching one or more channels.
type Subscription struct {
	SessionID string   `json:"session_id"`
	Kind      string   `json:"kind"`
	ThreadID  string   `json:"thread_id,omitempty"`
	Socket    string   `json:"socket,omitempty"`
	Token     string   `json:"token,omitempty"`
	Channels  []string `json:"channels"`
}

// Watches reports whether the subscription includes channel.
func (s *Subscription) Watches(channel string) bool { return slices.Contains(s.Channels, channel) }

// State is the listener's persisted state.
type State struct {
	JoinTS        map[string]string        `json:"join_ts"`
	Subscriptions map[string]*Subscription `json:"subscriptions"`
	Delivered     map[string]int64         `json:"delivered"`
}

// LoadState reads path; a missing file yields empty state.
func LoadState(path string) (*State, error) {
	s := &State{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	if s.JoinTS == nil {
		s.JoinTS = map[string]string{}
	}
	if s.Subscriptions == nil {
		s.Subscriptions = map[string]*Subscription{}
	}
	if s.Delivered == nil {
		s.Delivered = map[string]int64{}
	}
	return s, nil
}

// Save writes the state atomically with owner-only permissions.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func deliveredKey(session, channel, ts string) string { return session + "|" + channel + "|" + ts }

// WasDelivered reports whether ts in channel was already pushed to session.
func (s *State) WasDelivered(session, channel, ts string) bool {
	_, ok := s.Delivered[deliveredKey(session, channel, ts)]
	return ok
}

// MarkDelivered records that ts in channel was pushed to session.
func (s *State) MarkDelivered(session, channel, ts string, now time.Time) {
	s.Delivered[deliveredKey(session, channel, ts)] = now.Unix()
}

// Prune forgets deliveries older than the retention period.
func (s *State) Prune(now time.Time) {
	cutoff := now.Add(-deliveredRetention).Unix()
	for k, t := range s.Delivered {
		if t < cutoff {
			delete(s.Delivered, k)
		}
	}
}

// Watchers returns the subscriptions that include channel, ordered by session ID.
func (s *State) Watchers(channel string) []*Subscription {
	var out []*Subscription
	for _, sub := range s.Subscriptions {
		if sub.Watches(channel) {
			out = append(out, sub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// NowTS formats t as a Slack timestamp.
func NowTS(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }

func splitTS(ts string) (int64, int64) {
	sec, frac, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	f, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return s, f
}

// TSLess reports whether Slack timestamp a is earlier than b.
func TSLess(a, b string) bool {
	as, af := splitTS(a)
	bs, bf := splitTS(b)
	return as < bs || (as == bs && af < bf)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/agentchat/ -run 'TestState|TestWatchers|TestTS' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/agentchat/state.go pkg/agentchat/state_test.go
git commit -m "Add agent-chat persisted state"
```

---

### Task 5: Session delivery (Claude inbox, Codex app-server, queue fallback)

**Files:**
- Create: `pkg/agentchat/claude.go`, `pkg/agentchat/codex.go`, `pkg/agentchat/deliver.go`
- Test: `pkg/agentchat/claude_test.go`, `pkg/agentchat/codex_test.go`

**Interfaces:**
- Consumes: `Subscription`, `KindCodex`, `KindClaude` (Task 4).
- Produces: `var ErrSessionGone, ErrThreadNotLoaded, ErrCodexUnavailable error`; `DeliverClaude(ctx, socket, token, text string) error`; `DeliverCodex(ctx, socket, threadID, clientMsgID, text string) error`; `QueueCodex(ctx, codexHome, threadID, text string) error`; `type Deliverer interface{ Deliver(ctx context.Context, sub *Subscription, clientMsgID, text string) error }`; `type HostDeliverer struct{ CodexSocket, CodexHome string }`; `clientMessageID(session, channel, ts string) string`; test helper `shortSocketPath(t *testing.T) string`.

- [ ] **Step 1: Write the failing Claude test**

`pkg/agentchat/claude_test.go`:

```go
package agentchat

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortSocketPath returns a Unix socket path short enough for macOS.
func shortSocketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "sac")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestDeliverClaude(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer ln.Close()
	lines := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		var got []string
		for sc.Scan() {
			got = append(got, sc.Text())
		}
		lines <- got
	}()

	require.NoError(t, DeliverClaude(context.Background(), path, "tok", "hello\nworld"))

	got := <-lines
	require.Len(t, got, 2)
	var auth map[string]string
	require.NoError(t, json.Unmarshal([]byte(got[0]), &auth))
	assert.Equal(t, map[string]string{"type": "auth", "token": "tok"}, auth)
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(got[1]), &msg))
	assert.Equal(t, "user", msg.Type)
	assert.Equal(t, "user", msg.Message.Role)
	assert.Equal(t, "hello\nworld", msg.Message.Content)
}

func TestDeliverClaudeGone(t *testing.T) {
	err := DeliverClaude(context.Background(), shortSocketPath(t), "tok", "x")
	assert.ErrorIs(t, err, ErrSessionGone)
}
```

- [ ] **Step 2: Write the failing Codex test**

`pkg/agentchat/codex_test.go`:

```go
package agentchat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rpcCall struct {
	Method string
	Params map[string]any
}

// fakeCodex serves the app-server protocol on a Unix socket. respond returns
// (result, errorMessage) per call; a non-empty errorMessage becomes a JSON-RPC error.
type fakeCodex struct {
	mu      sync.Mutex
	calls   []rpcCall
	respond func(method string, params map[string]any) (any, string)
}

func (f *fakeCodex) start(t *testing.T) string {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	up := websocket.Upgrader{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			var req struct {
				ID     *int64         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := ws.ReadJSON(&req); err != nil {
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, rpcCall{req.Method, req.Params})
			f.mu.Unlock()
			if req.ID == nil {
				continue
			}
			// Interleave a notification to prove the client skips it.
			_ = ws.WriteJSON(map[string]any{"method": "thread/status/changed", "params": map[string]any{}})
			result, errMsg := f.respond(req.Method, req.Params)
			if errMsg != "" {
				_ = ws.WriteJSON(map[string]any{"id": *req.ID, "error": map[string]any{"code": -32600, "message": errMsg}})
			} else {
				_ = ws.WriteJSON(map[string]any{"id": *req.ID, "result": result})
			}
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func (f *fakeCodex) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
}

func (f *fakeCodex) last(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			return f.calls[i].Params
		}
	}
	return nil
}

func status(s string) any { return map[string]any{"thread": map[string]any{"id": "th", "status": map[string]any{"type": s}}} }

func TestDeliverCodexIdleStartsTurn(t *testing.T) {
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		switch m {
		case "thread/read":
			return status("idle"), ""
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "t1"}}, ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	require.NoError(t, DeliverCodex(context.Background(), sock, "th", "cid", "hi"))
	assert.Equal(t, []string{"initialize", "initialized", "thread/read", "turn/start"}, f.methods())
	p := f.last("turn/start")
	assert.Equal(t, "th", p["threadId"])
	assert.Equal(t, "cid", p["clientUserMessageId"])
	raw, _ := json.Marshal(p["input"])
	assert.JSONEq(t, `[{"type":"text","text":"hi"}]`, string(raw))
}

func TestDeliverCodexActiveSteers(t *testing.T) {
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		switch m {
		case "thread/read":
			return status("active"), ""
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": "turn-9", "status": "inProgress"}}}, ""
		case "turn/steer":
			return map[string]any{"turnId": "turn-9"}, ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	require.NoError(t, DeliverCodex(context.Background(), sock, "th", "cid", "hi"))
	assert.Equal(t, "turn-9", f.last("turn/steer")["expectedTurnId"])
	lp := f.last("thread/turns/list")
	assert.Equal(t, "desc", lp["sortDirection"])
	assert.EqualValues(t, 1, lp["limit"])
}

func TestDeliverCodexRetriesAfterSteerRejected(t *testing.T) {
	reads := 0
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		switch m {
		case "thread/read":
			reads++
			if reads == 1 {
				return status("active"), ""
			}
			return status("idle"), ""
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": "turn-9", "status": "inProgress"}}}, ""
		case "turn/steer":
			return nil, "no active turn"
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "t2"}}, ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	require.NoError(t, DeliverCodex(context.Background(), sock, "th", "cid", "hi"))
	assert.Equal(t, []string{"initialize", "initialized", "thread/read", "thread/turns/list", "turn/steer", "thread/read", "turn/start"}, f.methods())
}

func TestDeliverCodexNotLoaded(t *testing.T) {
	f := &fakeCodex{respond: func(m string, _ map[string]any) (any, string) {
		if m == "thread/read" {
			return status("notLoaded"), ""
		}
		return map[string]any{}, ""
	}}
	sock := f.start(t)
	assert.ErrorIs(t, DeliverCodex(context.Background(), sock, "th", "cid", "hi"), ErrThreadNotLoaded)
}

func TestDeliverCodexUnavailable(t *testing.T) {
	assert.ErrorIs(t, DeliverCodex(context.Background(), shortSocketPath(t), "th", "cid", "hi"), ErrCodexUnavailable)
}

func TestClientMessageIDDeterministic(t *testing.T) {
	a := clientMessageID("s", "C", "1.0")
	assert.Equal(t, a, clientMessageID("s", "C", "1.0"))
	assert.NotEqual(t, a, clientMessageID("s", "C", "2.0"))
	assert.Contains(t, a, "slack-agent-chat-")
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./pkg/agentchat/ -run 'TestDeliverClaude|TestDeliverCodex|TestClientMessageID' -v`
Expected: FAIL — `undefined: DeliverClaude`.

- [ ] **Step 4: Implement Claude delivery**

`pkg/agentchat/claude.go`:

```go
package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// ErrSessionGone means the target session no longer exists.
var ErrSessionGone = errors.New("session is gone")

// DeliverClaude posts text into a Claude Code session through its inbox
// socket: an auth line with the session token, then one user message line.
func DeliverClaude(ctx context.Context, socket, token, text string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w: %v", ErrSessionGone, err)
		}
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	auth, err := json.Marshal(map[string]string{"type": "auth", "token": token})
	if err != nil {
		return err
	}
	msg, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]string{"role": "user", "content": text},
	})
	if err != nil {
		return err
	}
	payload := append(append(append(auth, '\n'), msg...), '\n')
	_, err = conn.Write(payload)
	return err
}
```

- [ ] **Step 5: Implement Codex delivery**

`pkg/agentchat/codex.go`:

```go
package agentchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
)

var (
	// ErrThreadNotLoaded means the thread exists but is not loaded on the daemon.
	ErrThreadNotLoaded = errors.New("codex thread is not loaded on the app-server daemon")
	// ErrCodexUnavailable means the app-server daemon could not be reached.
	ErrCodexUnavailable = errors.New("codex app-server is unavailable")
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message) }

type codexConn struct {
	ws     *websocket.Conn
	nextID int64
}

func dialCodex(ctx context.Context, socket string) (*codexConn, error) {
	d := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "unix", socket)
		},
		HandshakeTimeout: 10 * time.Second,
	}
	ws, _, err := d.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCodexUnavailable, err)
	}
	c := &codexConn{ws: ws}
	init := map[string]any{"clientInfo": map[string]string{"name": "slack_agent_chat", "title": "Slack Agent Chat", "version": "1.0.0"}}
	if err := c.call(ctx, "initialize", init, nil); err != nil {
		ws.Close()
		return nil, fmt.Errorf("%w: initialize: %v", ErrCodexUnavailable, err)
	}
	if err := ws.WriteJSON(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		ws.Close()
		return nil, fmt.Errorf("%w: initialized: %v", ErrCodexUnavailable, err)
	}
	return c, nil
}

// call sends one request and waits for its response, skipping notifications
// and server-initiated requests.
func (c *codexConn) call(ctx context.Context, method string, params, result any) error {
	c.nextID++
	id := c.nextID
	if dl, ok := ctx.Deadline(); ok {
		_ = c.ws.SetReadDeadline(dl)
		_ = c.ws.SetWriteDeadline(dl)
	}
	if err := c.ws.WriteJSON(map[string]any{"method": method, "id": id, "params": params}); err != nil {
		return err
	}
	for {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := c.ws.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Method != "" || msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return msg.Error
		}
		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	}
}

// DeliverCodex pushes text into a Codex thread: turn/steer while a turn is
// running, turn/start when idle. A JSON-RPC rejection (the turn ended between
// the status check and the request) re-checks the status and retries.
func DeliverCodex(ctx context.Context, socket, threadID, clientMsgID, text string) error {
	c, err := dialCodex(ctx, socket)
	if err != nil {
		return err
	}
	defer c.ws.Close()
	input := []map[string]string{{"type": "text", "text": text}}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var read struct {
			Thread struct {
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"thread"`
		}
		if err := c.call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &read); err != nil {
			return err
		}
		switch read.Thread.Status.Type {
		case "notLoaded":
			return ErrThreadNotLoaded
		case "idle":
			lastErr = c.call(ctx, "turn/start", map[string]any{
				"threadId": threadID, "input": input, "clientUserMessageId": clientMsgID,
			}, nil)
		case "active":
			var turns struct {
				Data []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"data"`
			}
			if err := c.call(ctx, "thread/turns/list", map[string]any{
				"threadId": threadID, "sortDirection": "desc", "itemsView": "notLoaded", "limit": 1,
			}, &turns); err != nil {
				return err
			}
			if len(turns.Data) == 0 || turns.Data[0].Status != "inProgress" {
				lastErr = errors.New("active thread has no in-progress turn")
				continue
			}
			lastErr = c.call(ctx, "turn/steer", map[string]any{
				"threadId": threadID, "input": input,
				"expectedTurnId": turns.Data[0].ID, "clientUserMessageId": clientMsgID,
			}, nil)
		default:
			return fmt.Errorf("codex thread status %q", read.Thread.Status.Type)
		}
		if lastErr == nil {
			return nil
		}
		var rerr *rpcError
		if !errors.As(lastErr, &rerr) {
			return lastErr
		}
	}
	return lastErr
}

// QueueCodex hands text to `codex queue`, which delivers it once the thread is idle.
func QueueCodex(ctx context.Context, codexHome, threadID, text string) error {
	cmd := exec.CommandContext(ctx, "codex", "queue", "--thread", threadID, "--message", text)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex queue: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
```

- [ ] **Step 6: Implement the host deliverer**

`pkg/agentchat/deliver.go`:

```go
package agentchat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Deliverer pushes notice text into one session.
type Deliverer interface {
	Deliver(ctx context.Context, sub *Subscription, clientMsgID, text string) error
}

// HostDeliverer delivers to real Codex and Claude Code sessions.
type HostDeliverer struct {
	CodexSocket string
	CodexHome   string
}

// Deliver routes by session kind; Codex falls back to `codex queue` when the
// daemon is unreachable or the thread is not loaded on it.
func (d *HostDeliverer) Deliver(ctx context.Context, sub *Subscription, clientMsgID, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch sub.Kind {
	case KindClaude:
		return DeliverClaude(ctx, sub.Socket, sub.Token, text)
	case KindCodex:
		err := DeliverCodex(ctx, d.CodexSocket, sub.ThreadID, clientMsgID, text)
		if errors.Is(err, ErrThreadNotLoaded) || errors.Is(err, ErrCodexUnavailable) {
			return QueueCodex(ctx, d.CodexHome, sub.ThreadID, text)
		}
		return err
	}
	return fmt.Errorf("unknown subscription kind %q", sub.Kind)
}

// clientMessageID is a stable ID for pushing message ts in channel to session.
func clientMessageID(session, channel, ts string) string {
	return "slack-agent-chat-" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(session+"\x00"+channel+"\x00"+ts)).String()
}
```

- [ ] **Step 7: Run tests**

Run: `go get github.com/gorilla/websocket@v1.5.3 && go test ./pkg/agentchat/ -run 'TestDeliverClaude|TestDeliverCodex|TestClientMessageID' -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add go.mod go.sum pkg/agentchat/claude.go pkg/agentchat/codex.go pkg/agentchat/deliver.go pkg/agentchat/claude_test.go pkg/agentchat/codex_test.go
git commit -m "Deliver notices to Claude inbox sockets and Codex app-server threads"
```

---

### Task 6: Control socket

**Files:**
- Create: `pkg/agentchat/control.go`
- Test: `pkg/agentchat/control_test.go`

**Interfaces:**
- Consumes: `Subscription` (Task 4), `shortSocketPath` (Task 5 test helper).
- Produces: `type ControlRequest struct{ Op string; Subscription *Subscription; Backlog int; SessionID, Channel, TS string }` (JSON `op, subscription, backlog, session_id, channel, ts`); `type ControlResponse struct{ OK bool; Error string; Sessions []SessionStatus }` (JSON `ok, error, sessions`); `type SessionStatus struct{ SessionID, Kind string; Channels []string }` (JSON `session_id, kind, channels`); `type ControlHandler func(context.Context, ControlRequest) ControlResponse`; `var ErrListenerRunning`; `ListenControl(socket string) (net.Listener, error)`; `ServeControl(ctx context.Context, ln net.Listener, h ControlHandler)`; `SendControl(ctx context.Context, socket string, req ControlRequest) (ControlResponse, error)`.

- [ ] **Step 1: Write the failing test**

```go
package agentchat

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlRoundTrip(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := ListenControl(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ServeControl(ctx, ln, func(_ context.Context, req ControlRequest) ControlResponse {
		if req.Op == "status" {
			return ControlResponse{OK: true, Sessions: []SessionStatus{{SessionID: "s", Kind: KindCodex, Channels: []string{"C1"}}}}
		}
		return ControlResponse{Error: "unknown op " + req.Op}
	})

	resp, err := SendControl(ctx, path, ControlRequest{Op: "status"})
	require.NoError(t, err)
	assert.Equal(t, "s", resp.Sessions[0].SessionID)

	_, err = SendControl(ctx, path, ControlRequest{Op: "bogus"})
	assert.ErrorContains(t, err, "unknown op bogus")

	_, err = ListenControl(path)
	assert.ErrorIs(t, err, ErrListenerRunning)
}

func TestListenControlReplacesStaleSocket(t *testing.T) {
	path := shortSocketPath(t)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	ln, err := ListenControl(path)
	require.NoError(t, err)
	ln.Close()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run 'TestControl|TestListenControl' -v`
Expected: FAIL — `undefined: ListenControl`.

- [ ] **Step 3: Write minimal implementation**

`pkg/agentchat/control.go`:

```go
package agentchat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ErrListenerRunning means another listener already owns the control socket.
var ErrListenerRunning = errors.New("a listener is already running for this home")

// ControlRequest is one command sent to the listener.
type ControlRequest struct {
	Op           string        `json:"op"`
	Subscription *Subscription `json:"subscription,omitempty"`
	Backlog      int           `json:"backlog,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	Channel      string        `json:"channel,omitempty"`
	TS           string        `json:"ts,omitempty"`
}

// ControlResponse is the listener's reply.
type ControlResponse struct {
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Sessions []SessionStatus `json:"sessions,omitempty"`
}

// SessionStatus describes one subscribed session.
type SessionStatus struct {
	SessionID string   `json:"session_id"`
	Kind      string   `json:"kind"`
	Channels  []string `json:"channels"`
}

// ControlHandler answers one request.
type ControlHandler func(ctx context.Context, req ControlRequest) ControlResponse

// ListenControl binds the control socket, replacing a stale one.
func ListenControl(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		conn.Close()
		return nil, ErrListenerRunning
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// ServeControl answers one JSON line per connection until ctx ends.
func ServeControl(ctx context.Context, ln net.Listener, h ControlHandler) {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
			r := bufio.NewReader(conn)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req ControlRequest
			resp := ControlResponse{Error: "invalid request"}
			if json.Unmarshal(line, &req) == nil {
				resp = h(ctx, req)
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
		}(conn)
	}
}

// SendControl sends req to the listener and returns its reply; a reply with
// ok=false is returned as an error.
func SendControl(ctx context.Context, socket string, req ControlRequest) (ControlResponse, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return ControlResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	data, err := json.Marshal(req)
	if err != nil {
		return ControlResponse{}, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return ControlResponse{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return ControlResponse{}, err
	}
	var resp ControlResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return ControlResponse{}, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/agentchat/ -run 'TestControl|TestListenControl' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/agentchat/control.go pkg/agentchat/control_test.go
git commit -m "Add agent-chat listener control socket"
```

---

### Task 7: Listener core

**Files:**
- Create: `pkg/agentchat/listener.go`
- Test: `pkg/agentchat/listener_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–6.
- Produces: `type SlackAPI interface{…}` (methods below, all satisfied by `*slack.Client`); `type Listener struct{…}`; `NewListener(api SlackAPI, d Deliverer, self Identity, ownerID, stateFile string, log *zap.Logger) (*Listener, error)`; `(*Listener) HandleMessage(ctx, Message)`; `(*Listener) Subscribe(ctx, *Subscription, backlog int) error`; `(*Listener) Unsubscribe(sessionID, channel string)`; `(*Listener) Skip(sessionID, channel, ts string)`; `(*Listener) Status() []SessionStatus`; `(*Listener) HasSubscriptions() bool`; `(*Listener) Control(ctx, ControlRequest) ControlResponse`; `toMessage(channel string, m slack.Message) Message`; `const maxRecovery = 50`; reaction names `reactionDelivered = "eyes"`, `reactionAcked = "white_check_mark"`.

- [ ] **Step 1: Write the failing test**

`pkg/agentchat/listener_test.go`:

```go
package agentchat

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeSlack struct {
	mu        sync.Mutex
	users     map[string]*slack.User
	members   map[string][]string
	names     map[string]string
	history   map[string][]slack.Message
	replies   map[string][]slack.Message // key channel|thread_ts
	reactions []string                   // name|channel|ts
}

func newFakeSlack() *fakeSlack {
	u := func(id, name string, bot bool) *slack.User {
		return &slack.User{ID: id, Name: name, IsBot: bot, Profile: slack.UserProfile{DisplayName: name}}
	}
	return &fakeSlack{
		users: map[string]*slack.User{
			"UCL": u("UCL", "claude", true), "UCB": u("UCB", "codex-b", true),
			"UCR": u("UCR", "codex-r", true), "UBR": u("UBR", "brian", false),
		},
		members: map[string][]string{"C1": {"UCL", "UCB", "UCR", "UBR"}},
		names:   map[string]string{"C1": "proj"},
		history: map[string][]slack.Message{},
		replies: map[string][]slack.Message{},
	}
}

func (f *fakeSlack) AuthTestContext(context.Context) (*slack.AuthTestResponse, error) {
	return &slack.AuthTestResponse{UserID: "UCL", BotID: "BCL"}, nil
}
func (f *fakeSlack) GetUserInfoContext(_ context.Context, id string) (*slack.User, error) {
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, errors.New("user_not_found")
}
func (f *fakeSlack) GetUsersInConversationContext(_ context.Context, p *slack.GetUsersInConversationParameters) ([]string, string, error) {
	return f.members[p.ChannelID], "", nil
}
func (f *fakeSlack) GetConversationInfoContext(_ context.Context, in *slack.GetConversationInfoInput) (*slack.Channel, error) {
	ch := &slack.Channel{}
	ch.ID = in.ChannelID
	ch.Name = f.names[in.ChannelID]
	return ch, nil
}
func (f *fakeSlack) GetConversationHistoryContext(_ context.Context, p *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	var out []slack.Message
	for _, m := range f.history[p.ChannelID] { // stored newest first, like Slack
		if p.Oldest == "" || TSLess(p.Oldest, m.Timestamp) {
			out = append(out, m)
		}
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return &slack.GetConversationHistoryResponse{Messages: out}, nil
}
func (f *fakeSlack) GetConversationRepliesContext(_ context.Context, p *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return f.replies[p.ChannelID+"|"+p.Timestamp], false, "", nil
}
func (f *fakeSlack) AddReactionContext(_ context.Context, name string, item slack.ItemRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, name+"|"+item.Channel+"|"+item.Timestamp)
	return nil
}

type delivery struct{ session, clientID, text string }

type fakeDeliverer struct {
	mu   sync.Mutex
	got  []delivery
	errs map[string]error // by session
}

func (d *fakeDeliverer) Deliver(_ context.Context, sub *Subscription, clientID, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[sub.SessionID]; err != nil {
		return err
	}
	d.got = append(d.got, delivery{sub.SessionID, clientID, text})
	return nil
}

func newTestListener(t *testing.T, api *fakeSlack, d *fakeDeliverer) *Listener {
	l, err := NewListener(api, d, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	l.Now = func() time.Time { return time.Unix(2000, 0) }
	return l
}

func claudeSub(id string) *Subscription {
	return &Subscription{SessionID: id, Kind: KindClaude, Socket: "/s", Token: "t", Channels: []string{"C1"}}
}

func msg(ts, user, text string) slack.Message {
	m := slack.Message{}
	m.Timestamp, m.User, m.Text = ts, user, text
	return m
}

func TestListenerBroadcastDelivered(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))

	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.000001", User: "UCB", Text: "hi <@UCR> and all? no: just hi"})
	require.Len(t, d.got, 0, "mentions codex-r only")

	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.000002", User: "UBR", Text: "status please"})
	require.Len(t, d.got, 1)
	assert.Contains(t, d.got[0].text, "#proj (C1) from brian (the console user")
	assert.Equal(t, clientMessageID("s1", "C1", "2001.000002"), d.got[0].clientID)
	assert.Contains(t, api.reactions, "eyes|C1|2001.000002")
}

func TestListenerPlainNameMention(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UCB", Text: "@claude please review"})
	require.Len(t, d.got, 1)
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.2", User: "UCB", Text: "@codex-r please review"})
	assert.Len(t, d.got, 1)
}

func TestListenerIgnoresOwnAndEditsAndUnwatched(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UCL", BotID: "BCL", Text: "mine"})
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.2", User: "UCB", SubType: "message_changed", Text: "edit"})
	l.HandleMessage(context.Background(), Message{Channel: "C9", TS: "2001.3", User: "UCB", Text: "elsewhere"})
	assert.Empty(t, d.got)
}

func TestListenerDropsAgentRepeatsNotOwner(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UCB", Text: "same"})
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.2", User: "UCB", Text: "same"})
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.3", User: "UBR", Text: "again"})
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.4", User: "UBR", Text: "again"})
	assert.Len(t, d.got, 3)
}

func TestListenerDoesNotRedeliver(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	m := Message{Channel: "C1", TS: "2001.1", User: "UBR", Text: "once"}
	l.HandleMessage(context.Background(), m)
	l.HandleMessage(context.Background(), m)
	assert.Len(t, d.got, 1)
}

func TestListenerSkipSuppressesDelivery(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	l.Skip("s1", "C1", "2001.1")
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UBR", Text: "relayed"})
	require.Len(t, d.got, 1)
	assert.Equal(t, "s2", d.got[0].session)
}

func TestListenerDropsGoneClaudeSession(t *testing.T) {
	api := newFakeSlack()
	d := &fakeDeliverer{errs: map[string]error{"s1": fmt.Errorf("%w: gone", ErrSessionGone)}}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UBR", Text: "hello"})
	assert.False(t, l.HasSubscriptions())
}

func TestListenerBacklogOnFirstJoin(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	api.history["C1"] = []slack.Message{msg("1999.3", "UBR", "gamma"), msg("1999.2", "UCL", "mine-own"), msg("1999.1", "UCB", "alpha")}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 3))
	require.Len(t, d.got, 1)
	assert.Contains(t, d.got[0].text, "2 pending messages, oldest first")
	assert.Less(t, indexOf(d.got[0].text, "alpha"), indexOf(d.got[0].text, "gamma"))
	assert.NotContains(t, d.got[0].text, "mine-own")
}

func TestListenerRecoveryOnRejoin(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0)) // join point = 2000.000000

	acked := msg("2001.1", "UBR", "acked")
	acked.Reactions = []slack.ItemReaction{{Name: "white_check_mark", Users: []string{"UCL"}}}
	parent := msg("2001.2", "UCB", "parent")
	parent.ReplyCount, parent.LatestReply = 1, "2001.3"
	api.history["C1"] = []slack.Message{parent, acked, msg("1999.0", "UBR", "before join")}
	reply := msg("2001.3", "UBR", "thread reply")
	reply.ThreadTimestamp = "2001.2"
	api.replies["C1|2001.2"] = []slack.Message{parent, reply}

	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	require.Len(t, d.got, 1)
	assert.Equal(t, "s2", d.got[0].session)
	assert.Contains(t, d.got[0].text, "parent")
	assert.Contains(t, d.got[0].text, "thread reply")
	assert.NotContains(t, d.got[0].text, "acked")
	assert.NotContains(t, d.got[0].text, "before join")
}

func TestListenerStatusAndUnsubscribe(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	sub := claudeSub("s1")
	sub.Channels = []string{"C1", "C2"}
	require.NoError(t, l.Subscribe(context.Background(), sub, 0))
	l.Unsubscribe("s1", "C2")
	st := l.Status()
	require.Len(t, st, 1)
	assert.Equal(t, []string{"C1"}, st[0].Channels)
	l.Unsubscribe("s1", "")
	assert.False(t, l.HasSubscriptions())
}

func TestListenerSubscribeValidates(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	assert.Error(t, l.Subscribe(context.Background(), &Subscription{SessionID: "x", Kind: KindCodex, Channels: []string{"C1"}}, 0))
	assert.Error(t, l.Subscribe(context.Background(), &Subscription{SessionID: "x", Kind: KindClaude, Socket: "/s", Channels: []string{"C1"}}, 0))
	assert.Error(t, l.Subscribe(context.Background(), &Subscription{SessionID: "x", Kind: KindClaude, Socket: "/s", Token: "t"}, 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run 'TestListener' -v`
Expected: FAIL — `undefined: NewListener`.

- [ ] **Step 3: Write minimal implementation**

`pkg/agentchat/listener.go`:

```go
package agentchat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const (
	maxRecovery       = 50
	reactionDelivered = "eyes"
	reactionAcked     = "white_check_mark"
	repeatWindow      = 10 * time.Minute
	memberRefresh     = 5 * time.Minute
)

// SlackAPI is the part of *slack.Client the listener uses.
type SlackAPI interface {
	AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error)
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
	GetUsersInConversationContext(ctx context.Context, params *slack.GetUsersInConversationParameters) ([]string, string, error)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	AddReactionContext(ctx context.Context, name string, item slack.ItemRef) error
}

// Listener routes one home's Slack messages into its subscribed sessions.
type Listener struct {
	API       SlackAPI
	Deliverer Deliverer
	Self      Identity
	OwnerID   string
	StateFile string
	Now       func() time.Time
	Log       *zap.Logger

	mu       sync.Mutex
	state    *State
	repeats  *RepeatFilter
	users    map[string]*slack.User
	loadedAt map[string]time.Time
	chNames  map[string]string
}

type pending struct {
	msg    Message
	notice Notice
}

// NewListener loads persisted state and returns a ready listener.
func NewListener(api SlackAPI, d Deliverer, self Identity, ownerID, stateFile string, log *zap.Logger) (*Listener, error) {
	st, err := LoadState(stateFile)
	if err != nil {
		return nil, err
	}
	l := &Listener{
		API: api, Deliverer: d, Self: self, OwnerID: ownerID, StateFile: stateFile,
		Now: time.Now, Log: log, state: st,
		users: map[string]*slack.User{}, loadedAt: map[string]time.Time{}, chNames: map[string]string{},
	}
	l.repeats = NewRepeatFilter(repeatWindow, func() time.Time { return l.Now() })
	return l, nil
}

// --- directory ---

func (l *Listener) user(ctx context.Context, id string) *slack.User {
	if id == "" {
		return nil
	}
	l.mu.Lock()
	u, ok := l.users[id]
	l.mu.Unlock()
	if ok {
		return u
	}
	u, err := l.API.GetUserInfoContext(ctx, id)
	if err != nil {
		l.Log.Warn("users.info failed", zap.String("user", id), zap.Error(err))
		return nil
	}
	l.mu.Lock()
	l.users[id] = u
	l.mu.Unlock()
	return u
}

func (l *Listener) isAgent(ctx context.Context, id string) bool {
	u := l.user(ctx, id)
	return u != nil && u.IsBot
}

func (l *Listener) name(ctx context.Context, id string) string {
	u := l.user(ctx, id)
	switch {
	case u == nil:
		return ""
	case u.Profile.DisplayName != "":
		return u.Profile.DisplayName
	case u.Name != "":
		return u.Name
	}
	return u.RealName
}

// loadMembers caches the channel's members so plain @names resolve.
func (l *Listener) loadMembers(ctx context.Context, channel string) {
	l.mu.Lock()
	fresh := l.Now().Sub(l.loadedAt[channel]) < memberRefresh
	l.mu.Unlock()
	if fresh {
		return
	}
	cursor := ""
	for {
		ids, next, err := l.API.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{ChannelID: channel, Cursor: cursor, Limit: 200})
		if err != nil {
			l.Log.Warn("conversations.members failed", zap.String("channel", channel), zap.Error(err))
			return
		}
		for _, id := range ids {
			l.user(ctx, id)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	l.mu.Lock()
	l.loadedAt[channel] = l.Now()
	l.mu.Unlock()
}

func (l *Listener) resolve(name string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, u := range l.users {
		if u.IsBot && (strings.EqualFold(u.Name, name) || strings.EqualFold(u.Profile.DisplayName, name) || strings.EqualFold(u.RealName, name)) {
			return id
		}
	}
	return ""
}

func (l *Listener) channelName(ctx context.Context, channel string) string {
	l.mu.Lock()
	n, ok := l.chNames[channel]
	l.mu.Unlock()
	if ok {
		return n
	}
	ch, err := l.API.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channel})
	if err != nil {
		return channel
	}
	l.mu.Lock()
	l.chNames[channel] = ch.Name
	l.mu.Unlock()
	return ch.Name
}

// --- routing and delivery ---

// prepare applies the routing rule and builds m's notice.
func (l *Listener) prepare(ctx context.Context, m Message) (Notice, bool) {
	l.loadMembers(ctx, m.Channel)
	mentions := AgentMentions(m.Text, func(id string) bool { return l.isAgent(ctx, id) }, l.resolve)
	if !ShouldDeliver(m, l.Self, mentions) {
		return Notice{}, false
	}
	sender := l.name(ctx, m.User)
	if sender == "" {
		sender = "bot " + m.BotID
	}
	return Notice{
		ChannelID:   m.Channel,
		ChannelName: l.channelName(ctx, m.Channel),
		Sender:      sender,
		FromOwner:   m.User != "" && m.User == l.OwnerID,
		TS:          m.TS,
		ThreadTS:    m.ThreadTS,
		Text:        RenderMentions(m.Text, func(id string) string { return l.name(ctx, id) }),
		Files:       m.Files,
	}, true
}

// HandleMessage routes one live message to every subscribed session.
func (l *Listener) HandleMessage(ctx context.Context, m Message) {
	if !m.Deliverable() || m.From(l.Self) {
		return
	}
	l.mu.Lock()
	watchers := l.state.Watchers(m.Channel)
	l.mu.Unlock()
	if len(watchers) == 0 {
		return
	}
	if (m.BotID != "" || l.isAgent(ctx, m.User)) && l.repeats.Repeat(m) {
		l.Log.Info("dropped repeated agent message", zap.String("channel", m.Channel), zap.String("ts", m.TS), zap.String("user", m.User))
		return
	}
	n, ok := l.prepare(ctx, m)
	if !ok {
		return
	}
	for _, sub := range watchers {
		l.deliverTo(ctx, sub, []pending{{m, n}})
	}
}

// deliverTo pushes the items sub hasn't had yet as one notice, records them
// and marks each one delivered with a reaction.
func (l *Listener) deliverTo(ctx context.Context, sub *Subscription, items []pending) {
	l.mu.Lock()
	var fresh []pending
	for _, it := range items {
		if !l.state.WasDelivered(sub.SessionID, it.msg.Channel, it.msg.TS) {
			fresh = append(fresh, it)
		}
	}
	l.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	text := fresh[0].notice.Format()
	if len(fresh) > 1 {
		notices := make([]Notice, len(fresh))
		for i, it := range fresh {
			notices[i] = it.notice
		}
		text = FormatBatch(notices)
	}
	last := fresh[len(fresh)-1].msg
	if err := l.Deliverer.Deliver(ctx, sub, clientMessageID(sub.SessionID, last.Channel, last.TS), text); err != nil {
		l.Log.Warn("delivery failed", zap.String("session", sub.SessionID), zap.String("kind", sub.Kind), zap.Error(err))
		if errors.Is(err, ErrSessionGone) {
			l.Unsubscribe(sub.SessionID, "")
		}
		return
	}
	l.mu.Lock()
	for _, it := range fresh {
		l.state.MarkDelivered(sub.SessionID, it.msg.Channel, it.msg.TS, l.Now())
	}
	l.state.Prune(l.Now())
	err := l.state.Save(l.StateFile)
	l.mu.Unlock()
	if err != nil {
		l.Log.Error("saving state failed", zap.Error(err))
	}
	for _, it := range fresh {
		l.react(ctx, it.msg, reactionDelivered)
	}
}

func (l *Listener) react(ctx context.Context, m Message, name string) {
	err := l.API.AddReactionContext(ctx, name, slack.NewRefToMessage(m.Channel, m.TS))
	if err != nil && !strings.Contains(err.Error(), "already_reacted") {
		l.Log.Warn("adding reaction failed", zap.String("reaction", name), zap.String("ts", m.TS), zap.Error(err))
	}
}

// --- subscriptions ---

func validateSubscription(sub *Subscription) error {
	switch {
	case sub == nil || sub.SessionID == "":
		return errors.New("subscription needs a session ID")
	case len(sub.Channels) == 0:
		return errors.New("subscription needs at least one channel")
	case sub.Kind == KindCodex && sub.ThreadID == "":
		return errors.New("codex subscription needs a thread ID")
	case sub.Kind == KindClaude && (sub.Socket == "" || sub.Token == ""):
		return errors.New("claude subscription needs the messaging socket and token")
	case sub.Kind != KindCodex && sub.Kind != KindClaude:
		return fmt.Errorf("unknown subscription kind %q", sub.Kind)
	}
	return nil
}

// register records sub (merging channels with an existing subscription for
// the same session) and reports which channels this home is joining for the
// first time.
func (l *Listener) register(sub *Subscription) (*Subscription, map[string]bool, error) {
	if err := validateSubscription(sub); err != nil {
		return nil, nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	merged := *sub
	if old := l.state.Subscriptions[sub.SessionID]; old != nil {
		merged.Channels = slices.Clone(old.Channels)
		for _, ch := range sub.Channels {
			if !slices.Contains(merged.Channels, ch) {
				merged.Channels = append(merged.Channels, ch)
			}
		}
	}
	first := map[string]bool{}
	for _, ch := range sub.Channels {
		if _, ok := l.state.JoinTS[ch]; !ok {
			l.state.JoinTS[ch] = NowTS(l.Now())
			first[ch] = true
		}
	}
	l.state.Subscriptions[sub.SessionID] = &merged
	return &merged, first, l.state.Save(l.StateFile)
}

// recover pushes what sub should already have: the last backlog messages on
// a first join, or pending (unacknowledged) messages since the join point.
func (l *Listener) recover(ctx context.Context, sub *Subscription, channels []string, first map[string]bool, backlog int) {
	var items []pending
	for _, ch := range channels {
		var msgs []Message
		var err error
		if first[ch] {
			if backlog <= 0 {
				continue
			}
			msgs, err = l.lastMessages(ctx, ch, backlog)
		} else {
			l.mu.Lock()
			join := l.state.JoinTS[ch]
			l.mu.Unlock()
			msgs, err = l.pendingSince(ctx, ch, join)
		}
		if err != nil {
			l.Log.Warn("reading history failed", zap.String("channel", ch), zap.Error(err))
			continue
		}
		for _, m := range msgs {
			if !m.Deliverable() || m.From(l.Self) {
				continue
			}
			if n, ok := l.prepare(ctx, m); ok {
				items = append(items, pending{m, n})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return TSLess(items[i].msg.TS, items[j].msg.TS) })
	if len(items) > maxRecovery {
		items = items[len(items)-maxRecovery:]
	}
	if len(items) > 0 {
		l.deliverTo(ctx, sub, items)
	}
}

// Subscribe registers sub and delivers its backlog or pending messages.
func (l *Listener) Subscribe(ctx context.Context, sub *Subscription, backlog int) error {
	merged, first, err := l.register(sub)
	if err != nil {
		return err
	}
	l.recover(ctx, merged, sub.Channels, first, backlog)
	return nil
}

func (l *Listener) lastMessages(ctx context.Context, channel string, n int) ([]Message, error) {
	resp, err := l.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: channel, Limit: n})
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(resp.Messages))
	for i := len(resp.Messages) - 1; i >= 0; i-- {
		out = append(out, toMessage(channel, resp.Messages[i]))
	}
	return out, nil
}

// pendingSince returns messages after join (top level and thread replies)
// that this agent has not acknowledged.
func (l *Listener) pendingSince(ctx context.Context, channel, join string) ([]Message, error) {
	var out []Message
	keep := func(m slack.Message) {
		for _, r := range m.Reactions {
			if r.Name == reactionAcked && slices.Contains(r.Users, l.Self.UserID) {
				return
			}
		}
		out = append(out, toMessage(channel, m))
	}
	cursor := ""
	for page := 0; page < 5; page++ {
		resp, err := l.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: channel, Oldest: join, Cursor: cursor, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, m := range resp.Messages {
			if !TSLess(join, m.Timestamp) {
				continue
			}
			keep(m)
			if m.ReplyCount > 0 && TSLess(join, m.LatestReply) {
				replies, _, _, err := l.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channel, Timestamp: m.Timestamp, Oldest: join, Limit: 200})
				if err != nil {
					return nil, err
				}
				for _, r := range replies {
					if r.Timestamp != m.Timestamp && TSLess(join, r.Timestamp) {
						keep(r)
					}
				}
			}
		}
		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}
	return out, nil
}

func toMessage(channel string, m slack.Message) Message {
	files := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		files = append(files, f.Name)
	}
	return Message{Channel: channel, TS: m.Timestamp, ThreadTS: m.ThreadTimestamp, User: m.User, BotID: m.BotID, Text: m.Text, SubType: m.SubType, Files: files}
}

// Unsubscribe stops sessionID watching channel, or every channel when channel is "".
func (l *Listener) Unsubscribe(sessionID, channel string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sub := l.state.Subscriptions[sessionID]
	if sub == nil {
		return
	}
	if channel != "" {
		sub.Channels = slices.DeleteFunc(sub.Channels, func(c string) bool { return c == channel })
	}
	if channel == "" || len(sub.Channels) == 0 {
		delete(l.state.Subscriptions, sessionID)
	}
	if err := l.state.Save(l.StateFile); err != nil {
		l.Log.Error("saving state failed", zap.Error(err))
	}
}

// Skip records ts as already delivered to sessionID (used by %agents relays).
func (l *Listener) Skip(sessionID, channel, ts string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state.MarkDelivered(sessionID, channel, ts, l.Now())
	if err := l.state.Save(l.StateFile); err != nil {
		l.Log.Error("saving state failed", zap.Error(err))
	}
}

// Status lists subscribed sessions.
func (l *Listener) Status() []SessionStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []SessionStatus{}
	for _, sub := range l.state.Subscriptions {
		out = append(out, SessionStatus{SessionID: sub.SessionID, Kind: sub.Kind, Channels: slices.Clone(sub.Channels)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// HasSubscriptions reports whether any session is subscribed.
func (l *Listener) HasSubscriptions() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.state.Subscriptions) > 0
}

// Control answers control-socket requests. Subscribe replies once the
// subscription is recorded; recovery delivery continues in the background.
func (l *Listener) Control(ctx context.Context, req ControlRequest) ControlResponse {
	switch req.Op {
	case "subscribe":
		merged, first, err := l.register(req.Subscription)
		if err != nil {
			return ControlResponse{Error: err.Error()}
		}
		go l.recover(context.WithoutCancel(ctx), merged, req.Subscription.Channels, first, req.Backlog)
	case "unsubscribe":
		l.Unsubscribe(req.SessionID, req.Channel)
	case "skip":
		l.Skip(req.SessionID, req.Channel, req.TS)
	case "status":
	default:
		return ControlResponse{Error: "unknown op " + req.Op}
	}
	return ControlResponse{OK: true, Sessions: l.Status()}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./pkg/agentchat/ -run 'TestListener' -v`
Expected: PASS. If `TestListenerBroadcastDelivered` fails on the first message, check the text: it mentions `<@UCR>` only, so claude must not receive it.

- [ ] **Step 5: Run the whole package with the race detector**

Run: `go test -race ./pkg/agentchat/`
Expected: `ok`.

- [ ] **Step 6: Commit**

```bash
git add pkg/agentchat/listener.go pkg/agentchat/listener_test.go
git commit -m "Add agent-chat listener routing, delivery and recovery"
```

---

### Task 8: Socket Mode wiring and the listener daemon

**Files:**
- Create: `pkg/agentchat/daemon.go`
- Test: `pkg/agentchat/daemon_test.go`

**Interfaces:**
- Consumes: `Home` (Task 1), `NewListener`, `Listener.HandleMessage`, `Listener.Control`, `Listener.HasSubscriptions` (Task 7), `ListenControl`, `ServeControl` (Task 6), `HostDeliverer` (Task 5).
- Produces: `ParseEventsAPIMessage(payload []byte) (Message, bool)`; `RunListener(ctx context.Context, home Home, log *zap.Logger) error`; `const idleExit = 60 * time.Second`.

- [ ] **Step 1: Write the failing test**

```go
package agentchat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseEventsAPIMessage(t *testing.T) {
	payload := []byte(`{"type":"event_callback","event":{"type":"message","channel":"C1","user":"UBR","text":"hi","ts":"2.0","thread_ts":"1.0","files":[{"name":"a.png"}]}}`)
	m, ok := ParseEventsAPIMessage(payload)
	assert.True(t, ok)
	assert.Equal(t, Message{Channel: "C1", TS: "2.0", ThreadTS: "1.0", User: "UBR", Text: "hi", Files: []string{"a.png"}}, m)

	m, ok = ParseEventsAPIMessage([]byte(`{"event":{"type":"message","subtype":"bot_message","channel":"C1","bot_id":"B1","text":"x","ts":"3.0"}}`))
	assert.True(t, ok)
	assert.Equal(t, "B1", m.BotID)
	assert.Equal(t, "bot_message", m.SubType)

	_, ok = ParseEventsAPIMessage([]byte(`{"event":{"type":"reaction_added"}}`))
	assert.False(t, ok)
	_, ok = ParseEventsAPIMessage([]byte(`not json`))
	assert.False(t, ok)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run TestParseEventsAPIMessage -v`
Expected: FAIL — `undefined: ParseEventsAPIMessage`.

- [ ] **Step 3: Write minimal implementation**

`pkg/agentchat/daemon.go`:

```go
package agentchat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"
)

const idleExit = 60 * time.Second

// ParseEventsAPIMessage extracts a message event from a Socket Mode
// events_api payload; ok is false for any other event.
func ParseEventsAPIMessage(payload []byte) (Message, bool) {
	var env struct {
		Event struct {
			Type     string `json:"type"`
			SubType  string `json:"subtype"`
			Channel  string `json:"channel"`
			User     string `json:"user"`
			BotID    string `json:"bot_id"`
			Text     string `json:"text"`
			TS       string `json:"ts"`
			ThreadTS string `json:"thread_ts"`
			Files    []struct {
				Name string `json:"name"`
			} `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Event.Type != "message" {
		return Message{}, false
	}
	e := env.Event
	var files []string
	for _, f := range e.Files {
		files = append(files, f.Name)
	}
	return Message{Channel: e.Channel, TS: e.TS, ThreadTS: e.ThreadTS, User: e.User, BotID: e.BotID, Text: e.Text, SubType: e.SubType, Files: files}, true
}

func requireEnv(home Home, key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s is not set in %s", key, home.EnvFile)
	}
	return v, nil
}

// RunListener runs home's listener until ctx ends or no session has been
// subscribed for idleExit. The env file must already be loaded.
func RunListener(ctx context.Context, home Home, log *zap.Logger) error {
	appToken, err := requireEnv(home, "SLACK_MCP_XAPP_TOKEN")
	if err != nil {
		return err
	}
	botToken, err := requireEnv(home, "SLACK_MCP_XOXB_TOKEN")
	if err != nil {
		return err
	}
	userToken, err := requireEnv(home, "SLACK_MCP_XOXP_TOKEN")
	if err != nil {
		return err
	}

	ln, err := ListenControl(home.ControlSocket)
	if err != nil {
		return err
	}
	defer os.Remove(home.ControlSocket)

	botAPI := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	self, err := botAPI.AuthTestContext(ctx)
	if err != nil {
		ln.Close()
		return fmt.Errorf("bot auth.test: %w", err)
	}
	owner, err := slack.New(userToken).AuthTestContext(ctx)
	if err != nil {
		ln.Close()
		return fmt.Errorf("user auth.test: %w", err)
	}

	l, err := NewListener(botAPI, &HostDeliverer{CodexSocket: home.CodexSocket, CodexHome: home.Dir},
		Identity{UserID: self.UserID, BotID: self.BotID}, owner.UserID, home.StateFile, log)
	if err != nil {
		ln.Close()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go ServeControl(ctx, ln, l.Control)

	sm := socketmode.New(botAPI)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt := <-sm.Events:
				if evt.Type != socketmode.EventTypeEventsAPI || evt.Request == nil {
					continue
				}
				sm.Ack(*evt.Request)
				if m, ok := ParseEventsAPIMessage(evt.Request.Payload); ok {
					l.HandleMessage(ctx, m)
				}
			}
		}
	}()

	go func() {
		idleSince := time.Now()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				if l.HasSubscriptions() {
					idleSince = now
				} else if now.Sub(idleSince) >= idleExit {
					log.Info("no subscriptions; exiting")
					cancel()
					return
				}
			}
		}
	}()

	log.Info("listener started", zap.String("home", home.Dir), zap.String("bot_user", self.UserID), zap.String("owner", owner.UserID))
	err = sm.RunContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
```

- [ ] **Step 4: Run tests and build**

Run: `go test ./pkg/agentchat/ -run TestParseEventsAPIMessage -v && go build ./...`
Expected: PASS and a clean build.

- [ ] **Step 5: Commit**

```bash
git add pkg/agentchat/daemon.go pkg/agentchat/daemon_test.go
git commit -m "Run the agent-chat listener on Slack Socket Mode"
```

---

### Task 9: `chat` CLI

**Files:**
- Create: `pkg/agentchat/cli.go`
- Test: `pkg/agentchat/cli_test.go`
- Modify: `cmd/slack-mcp-server/main.go` (dispatch `chat` before flag parsing)

**Interfaces:**
- Consumes: `ResolveEnvFile`, `LoadEnvFile`, `NewHome` (Task 1); `SendControl`, `ControlRequest`, `SessionStatus` (Task 6); `Subscription`, kinds (Task 4); `RunListener` (Task 8); `reactionAcked` (Task 7).
- Produces: `RunCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int`; `NormalizeChannelName(string) (string, error)`; `ParseRelayPrompt(prompt string) (target, text string, ok bool)`; `detectSession(getenv func(string) string) (*Subscription, error)`; `relayContext(channelName, ts string) string`.

Command reference (all accept `--env-file` before the command):

```
slack-mcp-server chat [--env-file F] listen
slack-mcp-server chat watch start --channel C [--channel C2] [--backlog N]
slack-mcp-server chat watch stop [--channel C]
slack-mcp-server chat watch status
slack-mcp-server chat channel create NAME [--invite codex-r,claude]
slack-mcp-server chat channel invite CHANNEL AGENT[,AGENT]
slack-mcp-server chat post --channel C --text T [--thread TS]      (posts as the owner)
slack-mcp-server chat ack CHANNEL TS
slack-mcp-server chat relay-hook                                    (UserPromptSubmit hook; JSON on stdin)
```

Channel arguments accept an ID (`C…`/`G…`) or a name with or without `#`.

- [ ] **Step 1: Write the failing test**

```go
package agentchat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeChannelName(t *testing.T) {
	got, err := NormalizeChannelName("2026.09.21.Backup Restore")
	require.NoError(t, err)
	assert.Equal(t, "2026-09-21-backup-restore", got)
	got, err = NormalizeChannelName("#Proj__X")
	require.NoError(t, err)
	assert.Equal(t, "proj__x", got)
	long, err := NormalizeChannelName(strings.Repeat("a", 90))
	require.NoError(t, err)
	assert.Len(t, long, 80)
	_, err = NormalizeChannelName("...")
	assert.Error(t, err)
}

func TestParseRelayPrompt(t *testing.T) {
	target, text, ok := ParseRelayPrompt("  %agents: ship it\nnow")
	assert.True(t, ok)
	assert.Equal(t, "", target)
	assert.Equal(t, "ship it\nnow", text)

	target, text, ok = ParseRelayPrompt("%agents@proj: hi")
	assert.True(t, ok)
	assert.Equal(t, "proj", target)
	assert.Equal(t, "hi", text)

	_, _, ok = ParseRelayPrompt("please %agents: no")
	assert.False(t, ok)
}

func TestDetectSession(t *testing.T) {
	sub, err := detectSession(envMap(map[string]string{"CODEX_THREAD_ID": "th"}))
	require.NoError(t, err)
	assert.Equal(t, &Subscription{SessionID: "th", Kind: KindCodex, ThreadID: "th"}, sub)

	sub, err = detectSession(envMap(map[string]string{
		"CLAUDE_CODE_SESSION_ID": "cs", "CLAUDE_CODE_MESSAGING_SOCKET": "/s", "CLAUDE_CODE_MESSAGING_TOKEN": "tok",
	}))
	require.NoError(t, err)
	assert.Equal(t, &Subscription{SessionID: "cs", Kind: KindClaude, Socket: "/s", Token: "tok"}, sub)

	_, err = detectSession(envMap(map[string]string{"CLAUDE_CODE_SESSION_ID": "cs"}))
	assert.ErrorContains(t, err, "CLAUDE_CODE_MESSAGING_SOCKET")
	_, err = detectSession(envMap(nil))
	assert.Error(t, err)
}

func TestRelayContext(t *testing.T) {
	got := relayContext("proj", "5.0")
	assert.Contains(t, got, "#proj")
	assert.Contains(t, got, "do not send it again")
	assert.Contains(t, got, "Carry out that text yourself")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/agentchat/ -run 'TestNormalizeChannelName|TestParseRelayPrompt|TestDetectSession|TestRelayContext' -v`
Expected: FAIL — `undefined: NormalizeChannelName`.

- [ ] **Step 3: Write the implementation**

`pkg/agentchat/cli.go`:

```go
package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

var (
	channelIDRe   = regexp.MustCompile(`^[CG][A-Z0-9]{6,}$`)
	invalidNameRe = regexp.MustCompile(`[^a-z0-9_-]+`)
	relayRe       = regexp.MustCompile(`(?s)^\s*%agents(?:@([^:\s]*))?:(.*)$`)
)

// NormalizeChannelName converts name to a valid Slack channel name.
func NormalizeChannelName(name string) (string, error) {
	n := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	n = strings.Trim(invalidNameRe.ReplaceAllString(n, "-"), "-")
	if len(n) > 80 {
		n = strings.TrimRight(n[:80], "-")
	}
	if n == "" {
		return "", fmt.Errorf("%q has no usable characters for a channel name", name)
	}
	return n, nil
}

// ParseRelayPrompt recognizes "%agents: text" and "%agents@target: text".
func ParseRelayPrompt(prompt string) (target, text string, ok bool) {
	m := relayRe.FindStringSubmatch(prompt)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// detectSession identifies the calling Codex or Claude Code session.
func detectSession(getenv func(string) string) (*Subscription, error) {
	if th := getenv("CODEX_THREAD_ID"); th != "" {
		return &Subscription{SessionID: th, Kind: KindCodex, ThreadID: th}, nil
	}
	if sid := getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		sock, tok := getenv("CLAUDE_CODE_MESSAGING_SOCKET"), getenv("CLAUDE_CODE_MESSAGING_TOKEN")
		if sock == "" || tok == "" {
			return nil, errors.New("CLAUDE_CODE_MESSAGING_SOCKET and CLAUDE_CODE_MESSAGING_TOKEN must be set; cross-session messaging is unavailable in this session")
		}
		return &Subscription{SessionID: sid, Kind: KindClaude, Socket: sock, Token: tok}, nil
	}
	return nil, errors.New("not running inside a Codex or Claude Code session")
}

func relayContext(channelName, ts string) string {
	return fmt.Sprintf("slack-agent-chat relay: the console user typed this prompt with the %%agents prefix. "+
		"The hook already posted the text after the prefix to #%s as the user (ts %s); do not send it again. "+
		"Carry out that text yourself as the user's instruction.", channelName, ts)
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// parseArgs parses flags that may appear before or after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type cli struct {
	home   Home
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	bot    *slack.Client
	user   *slack.Client
}

// RunCLI runs `slack-mcp-server chat ARGS` and returns the exit code.
func RunCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envFile := fs.String("env-file", "", "path to slack-mcp-server.env")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: slack-mcp-server chat [--env-file F] listen|watch|channel|post|ack|relay-hook ...")
		return 2
	}
	path, err := ResolveEnvFile(*envFile, os.Getenv)
	if err == nil {
		err = LoadEnvFile(path)
	}
	if err != nil {
		if rest[0] == "relay-hook" {
			return emitBlock(stdout, err)
		}
		fmt.Fprintf(stderr, "slack-mcp-server chat: %v\n", err)
		return 1
	}
	c := &cli{home: NewHome(path), stdin: stdin, stdout: stdout, stderr: stderr,
		bot: slack.New(os.Getenv("SLACK_MCP_XOXB_TOKEN")), user: slack.New(os.Getenv("SLACK_MCP_XOXP_TOKEN"))}
	ctx := context.Background()

	switch rest[0] {
	case "listen":
		err = c.listen()
	case "watch":
		err = c.watch(ctx, rest[1:])
	case "channel":
		err = c.channel(ctx, rest[1:])
	case "post":
		err = c.post(ctx, rest[1:])
	case "ack":
		err = c.ack(ctx, rest[1:])
	case "relay-hook":
		return c.relayHook(ctx)
	default:
		err = fmt.Errorf("unknown command %q", rest[0])
	}
	if err != nil {
		fmt.Fprintf(stderr, "slack-mcp-server chat: %v\n", err)
		return 1
	}
	return 0
}

func (c *cli) printJSON(v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintln(c.stdout, string(data))
}

func (c *cli) listen() error {
	log, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer log.Sync()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = RunListener(ctx, c.home, log)
	if errors.Is(err, ErrListenerRunning) {
		return nil
	}
	return err
}

// ensureListener starts the home's listener in the background if it is not running.
func (c *cli) ensureListener(ctx context.Context) error {
	if _, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "status"}); err == nil {
		return nil
	}
	if err := os.MkdirAll(c.home.StateDir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(c.home.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "chat", "--env-file", c.home.EnvFile, "listen")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 60; i++ {
		time.Sleep(250 * time.Millisecond)
		if _, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "status"}); err == nil {
			return nil
		}
	}
	return fmt.Errorf("listener did not start; see %s", c.home.LogFile)
}

// resolveChannel turns an ID or name into a channel ID and name.
func (c *cli) resolveChannel(ctx context.Context, arg string) (string, string, error) {
	if channelIDRe.MatchString(arg) {
		ch, err := c.bot.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: arg})
		if err != nil {
			return "", "", fmt.Errorf("channel %s: %w", arg, err)
		}
		return ch.ID, ch.Name, nil
	}
	name := strings.TrimPrefix(arg, "#")
	cursor := ""
	for {
		chans, next, err := c.bot.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
			Types: []string{"private_channel", "public_channel"}, ExcludeArchived: true, Limit: 200, Cursor: cursor,
		})
		if err != nil {
			return "", "", err
		}
		for _, ch := range chans {
			if ch.Name == name {
				return ch.ID, ch.Name, nil
			}
		}
		if next == "" {
			return "", "", fmt.Errorf("this agent's bot is not in a channel named %q", name)
		}
		cursor = next
	}
}

func (c *cli) subscribe(ctx context.Context, channelIDs []string, backlog int) error {
	sub, err := detectSession(os.Getenv)
	if err != nil {
		return err
	}
	sub.Channels = channelIDs
	if err := c.ensureListener(ctx); err != nil {
		return err
	}
	resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "subscribe", Subscription: sub, Backlog: backlog})
	if err != nil {
		return err
	}
	for _, s := range resp.Sessions {
		if s.SessionID == sub.SessionID {
			c.printJSON(map[string]any{"ok": true, "session": s.SessionID, "channels": s.Channels})
		}
	}
	return nil
}

func (c *cli) watch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: watch start|stop|status")
	}
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var channels stringList
	fs.Var(&channels, "channel", "channel ID or name (repeatable)")
	backlog := fs.Int("backlog", 0, "on first join, deliver this many recent messages")
	if _, err := parseArgs(fs, args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "start":
		if len(channels) == 0 {
			return errors.New("watch start needs --channel")
		}
		var ids []string
		for _, ch := range channels {
			id, _, err := c.resolveChannel(ctx, ch)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return c.subscribe(ctx, ids, *backlog)
	case "stop":
		sub, err := detectSession(os.Getenv)
		if err != nil {
			return err
		}
		channel := ""
		if len(channels) > 0 {
			if channel, _, err = c.resolveChannel(ctx, channels[0]); err != nil {
				return err
			}
		}
		resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "unsubscribe", SessionID: sub.SessionID, Channel: channel})
		if err != nil {
			return err
		}
		c.printJSON(map[string]any{"ok": true, "sessions": resp.Sessions})
		return nil
	case "status":
		resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "status"})
		if err != nil {
			c.printJSON(map[string]any{"running": false})
			return nil
		}
		c.printJSON(map[string]any{"running": true, "sessions": resp.Sessions})
		return nil
	}
	return fmt.Errorf("unknown watch command %q", args[0])
}

// lookupAgents maps agent names to bot user IDs.
func (c *cli) lookupAgents(ctx context.Context, names []string) ([]string, error) {
	users, err := c.bot.GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, name := range names {
		name = strings.TrimPrefix(strings.TrimSpace(name), "@")
		if name == "" {
			continue
		}
		found := ""
		for _, u := range users {
			if u.IsBot && !u.Deleted && (strings.EqualFold(u.Name, name) || strings.EqualFold(u.Profile.DisplayName, name) || strings.EqualFold(u.RealName, name)) {
				found = u.ID
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("no agent named %q", name)
		}
		ids = append(ids, found)
	}
	return ids, nil
}

func (c *cli) channel(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: channel create|invite")
	}
	fs := flag.NewFlagSet("channel", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	invite := fs.String("invite", "", "comma-separated agent names to invite")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		if len(pos) != 1 {
			return errors.New("usage: channel create NAME [--invite a,b]")
		}
		name, err := NormalizeChannelName(pos[0])
		if err != nil {
			return err
		}
		agentIDs, err := c.lookupAgents(ctx, strings.Split(*invite, ","))
		if err != nil {
			return err
		}
		owner, err := c.user.AuthTestContext(ctx)
		if err != nil {
			return fmt.Errorf("user auth.test: %w", err)
		}
		ch, err := c.bot.CreateConversationContext(ctx, slack.CreateConversationParams{ChannelName: name, IsPrivate: true})
		if err != nil {
			return fmt.Errorf("creating #%s: %w", name, err)
		}
		if _, err := c.bot.InviteUsersToConversationContext(ctx, ch.ID, append([]string{owner.UserID}, agentIDs...)...); err != nil {
			return fmt.Errorf("inviting to #%s: %w", name, err)
		}
		c.printJSON(map[string]any{"channel_id": ch.ID, "name": ch.Name})
		return c.subscribe(ctx, []string{ch.ID}, 0)
	case "invite":
		if len(pos) != 2 {
			return errors.New("usage: channel invite CHANNEL AGENT[,AGENT]")
		}
		id, name, err := c.resolveChannel(ctx, pos[0])
		if err != nil {
			return err
		}
		agentIDs, err := c.lookupAgents(ctx, strings.Split(pos[1], ","))
		if err != nil {
			return err
		}
		if _, err := c.bot.InviteUsersToConversationContext(ctx, id, agentIDs...); err != nil {
			return fmt.Errorf("inviting to #%s: %w", name, err)
		}
		c.printJSON(map[string]any{"ok": true, "channel_id": id, "invited": agentIDs})
		return nil
	}
	return fmt.Errorf("unknown channel command %q", args[0])
}

// postAsOwner posts text as the owner and returns the message ts.
func (c *cli) postAsOwner(ctx context.Context, channelID, thread, text string) (string, error) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false), slack.MsgOptionAsUser(true)}
	if thread != "" {
		opts = append(opts, slack.MsgOptionTS(thread))
	}
	_, ts, err := c.user.PostMessageContext(ctx, channelID, opts...)
	return ts, err
}

func (c *cli) post(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	channel := fs.String("channel", "", "channel ID or name")
	text := fs.String("text", "", "message text")
	thread := fs.String("thread", "", "thread ts")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if *channel == "" || *text == "" {
		return errors.New("post needs --channel and --text")
	}
	id, _, err := c.resolveChannel(ctx, *channel)
	if err != nil {
		return err
	}
	ts, err := c.postAsOwner(ctx, id, *thread, *text)
	if err != nil {
		return err
	}
	c.printJSON(map[string]any{"ok": true, "channel_id": id, "ts": ts})
	return nil
}

func (c *cli) ack(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: ack CHANNEL TS")
	}
	if _, err := strconv.ParseFloat(args[1], 64); err != nil {
		return fmt.Errorf("invalid ts %q", args[1])
	}
	err := c.bot.AddReactionContext(ctx, reactionAcked, slack.NewRefToMessage(args[0], args[1]))
	if err != nil && !strings.Contains(err.Error(), "already_reacted") {
		return err
	}
	c.printJSON(map[string]any{"ok": true})
	return nil
}

func emitBlock(w io.Writer, err error) int {
	data, _ := json.Marshal(map[string]string{"decision": "block", "reason": "slack-agent-chat relay: " + err.Error()})
	fmt.Fprintln(w, string(data))
	return 0
}

// relayHook implements the UserPromptSubmit hook for %agents prompts.
func (c *cli) relayHook(ctx context.Context) int {
	var event struct {
		Prompt    string `json:"prompt"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(c.stdin).Decode(&event); err != nil {
		return 0
	}
	target, text, ok := ParseRelayPrompt(event.Prompt)
	if !ok {
		return 0
	}
	if text == "" {
		return emitBlock(c.stdout, errors.New("nothing to relay after the %agents prefix"))
	}
	session := event.SessionID
	if session == "" {
		session = os.Getenv("CODEX_THREAD_ID")
	}
	channelID, channelName, err := c.relayTarget(ctx, session, target)
	if err != nil {
		return emitBlock(c.stdout, err)
	}
	ts, err := c.postAsOwner(ctx, channelID, "", text)
	if err != nil {
		return emitBlock(c.stdout, err)
	}
	if _, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "skip", SessionID: session, Channel: channelID, TS: ts}); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: could not mark relay as delivered: %v\n", err)
	}
	data, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "UserPromptSubmit", "additionalContext": relayContext(channelName, ts),
	}})
	fmt.Fprintln(c.stdout, string(data))
	return 0
}

// relayTarget picks the channel for a relay: the explicit target, or the one
// channel this session watches.
func (c *cli) relayTarget(ctx context.Context, session, target string) (string, string, error) {
	if target != "" {
		return c.resolveChannel(ctx, target)
	}
	resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "status"})
	if err != nil {
		return "", "", errors.New("no slack-agent-chat listener is running; start a watch or use %agents@<channel>:")
	}
	for _, s := range resp.Sessions {
		if s.SessionID != session {
			continue
		}
		if len(s.Channels) == 1 {
			return c.resolveChannel(ctx, s.Channels[0])
		}
		return "", "", fmt.Errorf("this session watches %d channels; use %%agents@<channel>: to pick one", len(s.Channels))
	}
	return "", "", errors.New("this session is not watching a channel; start a watch or use %agents@<channel>:")
}
```

- [ ] **Step 4: Dispatch `chat` in main**

In `cmd/slack-mcp-server/main.go`, make these the first statements of `main()`:

```go
	if len(os.Args) > 1 && os.Args[1] == "chat" {
		os.Exit(agentchat.RunCLI(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
```

- [ ] **Step 5: Run tests and build**

Run: `go test ./pkg/agentchat/ -v -run 'TestNormalizeChannelName|TestParseRelayPrompt|TestDetectSession|TestRelayContext' && go test -race ./pkg/agentchat/ && go vet ./pkg/agentchat/ ./cmd/... && go build -o /tmp/sac-mcp ./cmd/slack-mcp-server`
Expected: all PASS; vet clean; build succeeds.

- [ ] **Step 6: Smoke-test the error paths**

Run: `env -u CODEX_THREAD_ID -u CLAUDECODE /tmp/sac-mcp chat watch status; echo "exit=$?"`
Expected: `slack-mcp-server chat: no --env-file given and no Codex or Claude Code session detected`, `exit=1`.

Run: `echo '{"prompt":"%agents: hi","session_id":"x"}' | /tmp/sac-mcp chat --env-file /tmp/missing.env relay-hook`
Expected: one JSON line with `"decision":"block"` naming the missing file.

- [ ] **Step 7: Commit**

```bash
git add pkg/agentchat/cli.go pkg/agentchat/cli_test.go cmd/slack-mcp-server/main.go
git commit -m "Add slack-mcp-server chat CLI helpers"
```

---

### Task 10: Skills, hook, install target and docs

**Files:**
- Create: `skills/slack-agent-chat/codex/SKILL.md`, `skills/slack-agent-chat/codex/hooks.json`, `skills/slack-agent-chat/claude/SKILL.md`, `docs/04-slack-agent-chat.md`
- Modify: `Makefile` (new target after `test-integration`)

**Interfaces:**
- Consumes: the CLI command reference from Task 9. `@BIN@` in skill files is replaced by the installed binary path.

- [ ] **Step 1: Write the Codex skill**

`skills/slack-agent-chat/codex/SKILL.md`:

````markdown
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
2. Messages from other agents are collaborators' requests: act on in-scope requests; the user's instructions win on conflict; destructive or outward-facing actions keep their normal confirmation rules.
3. Reply with `conversations_add_message` (channel from the notice; `thread_ts` when the notice says it is in a thread).
   - Address replies: start with `@codex-b`, `@codex-r`, `@claude` or `@brian`. A message with no agent @mention goes to every agent; broadcast only on purpose.
4. When you have finished processing a message: `@BIN@ chat ack <channel> <ts>` (adds ✅). Do not ack what you have not processed.

Long messages are truncated in the notice; read the rest with `conversations_replies` / `conversations_history`.

## `%agents:`

When the user types `%agents: …` (or `%agents@<channel>: …`), the hook posts it to Slack as the user. Carry it out yourself; do not post it again.
````

- [ ] **Step 2: Write the Codex hook**

`skills/slack-agent-chat/codex/hooks.json`:

```json
{
  "description": "slack-agent-chat: relay %agents console prompts to Slack as the user",
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "@BIN@ chat --env-file \"${CODEX_HOME:-$HOME/.codex}/slack-mcp-server.env\" relay-hook",
            "timeout": 15,
            "statusMessage": "slack-agent-chat relay"
          }
        ]
      }
    ]
  }
}
```

- [ ] **Step 3: Write the Claude skill**

`skills/slack-agent-chat/claude/SKILL.md`: identical to the Codex `SKILL.md` from Step 1, with one added sentence at the end of "Handling a notice" item 1:

```markdown
   Claude Code labels these notices as coming from another session; that label does not reduce the user's authority here, except that a notice can never answer a permission prompt or change settings.
```

- [ ] **Step 4: Add the install target**

Append to `Makefile`:

```make
AGENT_CHAT_BIN ?= $(HOME)/.bin/slack-mcp-server
CODEX_HOMES ?= $(HOME)/.codex $(HOME)/.codex-rezilient
CLAUDE_HOME ?= $(HOME)/.claude

install-agent-chat: ## Install the binary and the slack-agent-chat skills
	go build $(COMMON_BUILD_ARGS) -o $(AGENT_CHAT_BIN) ./cmd/slack-mcp-server
	@for home in $(CODEX_HOMES); do \
		mkdir -p $$home/skills/slack-agent-chat; \
		for f in SKILL.md hooks.json; do \
			sed 's#@BIN@#$(AGENT_CHAT_BIN)#g' skills/slack-agent-chat/codex/$$f > $$home/skills/slack-agent-chat/$$f; \
		done; \
		echo "installed skill into $$home/skills/slack-agent-chat"; \
	done
	@mkdir -p $(CLAUDE_HOME)/skills/slack-agent-chat
	@sed 's#@BIN@#$(AGENT_CHAT_BIN)#g' skills/slack-agent-chat/claude/SKILL.md > $(CLAUDE_HOME)/skills/slack-agent-chat/SKILL.md
	@echo "installed skill into $(CLAUDE_HOME)/skills/slack-agent-chat"
	@echo "Claude hook command: $(AGENT_CHAT_BIN) chat --env-file $(CLAUDE_HOME)/slack-mcp-server.env relay-hook"
```

Also add `install-agent-chat` to the `.PHONY` line if the Makefile has one.

- [ ] **Step 5: Write the setup doc**

`docs/04-slack-agent-chat.md`:

````markdown
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

Environment variables are no longer read; the file is the only source.

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

Codex picks up the skill's `UserPromptSubmit` hook on the next session and asks you to trust it.
For Claude Code, add the printed hook command under `hooks.UserPromptSubmit` in `~/.claude/settings.json`, and remove the old `agent-chat` relay hook there.

## Use

- Start a project chat: tell the agent to create the channel (it runs `chat channel create`).
- Join: `chat watch start --channel <name>`.
- From any session prompt: `%agents: …` posts to that session's channel as you.
- Listener state and log: `<home>/slack-agent-chat/`.
````

- [ ] **Step 6: Install and check the output**

Run: `make install-agent-chat && grep -c '@BIN@' ~/.codex/skills/slack-agent-chat/SKILL.md ~/.claude/skills/slack-agent-chat/SKILL.md || true`
Expected: three "installed skill" lines and the hook command; each grep count `0`.

- [ ] **Step 7: Commit**

```bash
git add skills/slack-agent-chat Makefile docs/04-slack-agent-chat.md
git commit -m "Add slack-agent-chat skills, hook and install target"
```

---

### Task 11: Live verification (with the user)

No code. Run after the user has completed the one-time Slack setup and created the three env files.

- [ ] **Step 1: Claude inbox delivery.** The user runs, in a Claude Code session with the skill installed: `chat channel create sac-smoke` (as the Claude agent). Expected: JSON with `channel_id`; `chat watch status` shows the session. The user posts "hello" in the channel from Slack. Expected: a `[slack-agent-chat] #sac-smoke … from brian (the console user…)` notice arrives in the Claude session, and Slack shows 👀 from `claude`.
- [ ] **Step 2: Codex delivery mid-turn.** In a `codex --remote unix://` session for `~/.codex`: `chat watch start --channel sac-smoke` after `chat channel invite sac-smoke codex-b` from the Claude session. While Codex is busy on a long command, the user posts in Slack. Expected: the notice arrives in the running turn (steer); with Codex idle, it starts a new turn.
- [ ] **Step 3: Routing.** Post `@codex-b only you` from Slack. Expected: only codex-b gets it.
- [ ] **Step 4: Relay.** In the Claude session type `%agents: relay test`. Expected: Slack shows the message from brian; codex-b gets the notice; the Claude session does not get it back.
- [ ] **Step 5: Ack and recovery.** Codex runs `chat ack <channel> <ts>` on one message. Restart the Codex session and run `watch start` again. Expected: only unacknowledged messages since the join are re-delivered, in one batch.
- [ ] **Step 6: Idle exit.** `chat watch stop` in every session. Expected: the listener process exits within ~65 s and `listener.sock` is gone.
