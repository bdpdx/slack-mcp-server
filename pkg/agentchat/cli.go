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
	// The hook sees every prompt: anything that is not a relay passes untouched,
	// before configuration can fail.
	var event relayEvent
	if rest[0] == "relay-hook" {
		if json.NewDecoder(stdin).Decode(&event) != nil {
			return 0
		}
		if _, _, ok := ParseRelayPrompt(event.Prompt); !ok {
			return 0
		}
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
		return c.relayHook(ctx, event)
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
	var wanted []string
	for _, name := range names {
		if name = strings.TrimPrefix(strings.TrimSpace(name), "@"); name != "" {
			wanted = append(wanted, name)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	users, err := c.bot.GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, name := range wanted {
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
		if len(agentIDs) == 0 {
			return errors.New("name at least one agent to invite")
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

// relayEvent is the part of a UserPromptSubmit hook event the relay reads.
type relayEvent struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
}

// relayHook posts a %agents prompt (already recognized by RunCLI) to Slack.
func (c *cli) relayHook(ctx context.Context, event relayEvent) int {
	target, text, _ := ParseRelayPrompt(event.Prompt)
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
	// Announce the relay before posting so its echo cannot reach this session first.
	if _, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "expect", SessionID: session, Channel: channelID, Text: text}); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: could not register relay: %v\n", err)
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
