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
	"slices"
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

// relayTimeout bounds everything the %agents relay hook does, so a stuck
// listener or a slow Slack never holds the user's prompt for long.
var relayTimeout = 10 * time.Second

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

const chatUsage = `usage: slack-mcp-server chat [--env-file FILE] COMMAND

  --env-file FILE   slack-mcp-server.env to use (default: detected from the
                    Codex or Claude Code session's home)

Commands (CHANNEL is an ID like C0123ABCD or a name like #proj):
  watch start --channel CHANNEL [--channel CHANNEL] [--backlog N]
                    push this session the channel's messages; for a project
                    channel, also set up and watch #PROJECT__USER_AGENT
  watch stop [--channel CHANNEL]
                    stop pushing one channel, or all of them
  watch status      list the sessions this home's listener serves
  channel create NAME [--invite AGENT,AGENT] [--invite-user USER,USER]
                    create a private project channel, invite the user and the
                    named agents and people, create #NAME__users for the
                    people, and watch it as with watch start
  channel invite CHANNEL [AGENT,AGENT] [--invite-user USER,USER]
                    add agents or people to a channel
  project archive NAME [--dry-run]
                    archive #NAME and every #NAME__* channel you are in; only
                    when Slack records you (or this agent) as #NAME's creator.
                    Slack's API cannot delete channels; archived ones can be
                    restored, or deleted in the Slack UI by a workspace owner
  side AGENT[,AGENT] [--channel PROJECT]
                    open (or join) the side channel #PROJECT__A_B with these
                    agents and watch it; PROJECT defaults to the project this
                    session watches
  post --channel CHANNEL --text TEXT [--thread TS]
                    post as the user (agents reply with conversations_add_message)
  ack CHANNEL TS    mark a message processed (adds a check-mark reaction)
  cohort register --project P --agent NAME [--project-root DIR]
                    join this session to projects/P's cohort: the listener
                    then tells it when the GM looks unavailable (it is next
                    in the succession order) and when its 2-hour checkpoint
                    is due. Validates P's PROJECT.md (exit 2) and that this
                    session watches #P (exit 3)
  cohort leave|duty on|off|checkpoint --drift LINE --project P
                    leave; go off or on duty (never lifts a user's hold);
                    record a checkpoint and post the drift line
  cohort status [--project P] [--format json|table]
                    list this home's cohort registrations
  gm status|init|claim|verify|release --project P [--agent NAME]
                    the project's GM authority (gm.json on project-state's
                    origin/main). init gives a new project's first GM term
                    1. claim --expect-term T --expect-gm G takes over by
                    compare-and-swap, only when this home's listener has a
                    due deadline reaching this agent's slot (or with
                    --user-directed --reason R); exit 4 lost or refused, 5
                    authority unavailable. verify --term T --claim-id ID
                    before each GM-file write (exit 4: stop); release --term
                    T --claim-id ID --to AGENT hands GM back
  capabilities      print this build's cohort capabilities as JSON
  relay-hook        UserPromptSubmit hook for %agents prompts (reads stdin)
  ask-hook          Claude PreToolUse hook for AskUserQuestion: ask in Slack
                    instead of the terminal while watching a project
  approval-hook [--wait 10m]
                    PermissionRequest hook: ask the user in Slack (buttons or
                    a thread reply) and answer the prompt; after --wait with
                    no answer, the terminal asks and the project channel
                    gets a BLOCKED notice
  stop-hook         Stop hook: DM the user the final response of a turn they
                    started at the terminal (relay-hook marks those turns)
  listen            run the listener in the foreground (started automatically)
`

// RunCLI runs `slack-mcp-server chat ARGS` and returns the exit code.
func RunCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, chatUsage) }
	envFile := fs.String("env-file", "", "path to slack-mcp-server.env")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, chatUsage)
		return 2
	}
	if rest[0] == "help" {
		fmt.Fprint(stdout, chatUsage)
		return 0
	}
	// The hook sees every prompt: anything that is not a relay passes untouched,
	// before configuration can fail.
	var event relayEvent
	if rest[0] == "relay-hook" {
		if json.NewDecoder(stdin).Decode(&event) != nil {
			return 0
		}
		markTurn(*envFile, event, stderr)
		if _, _, ok := ParseRelayPrompt(event.Prompt); !ok {
			return 0
		}
	}
	var hook hookEvent
	if isQuietHook(rest[0]) && json.NewDecoder(stdin).Decode(&hook) != nil {
		return 0
	}
	path, err := ResolveEnvFile(*envFile, os.Getenv)
	if err == nil {
		err = LoadEnvFile(path)
	}
	if err != nil {
		if rest[0] == "relay-hook" {
			if path != "" {
				ClearTurn(NewHome(path), sessionOf(event.SessionID))
			}
			return emitBlock(stdout, err)
		}
		if isQuietHook(rest[0]) {
			fmt.Fprintf(stderr, "slack-agent-chat: %v\n", err)
			return 0
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
	case "side":
		err = c.side(ctx, rest[1:])
	case "project":
		err = c.project(ctx, rest[1:])
	case "post":
		err = c.post(ctx, rest[1:])
	case "ack":
		err = c.ack(ctx, rest[1:])
	case "cohort":
		err = c.cohort(ctx, rest[1:])
	case "gm":
		err = c.gm(ctx, rest[1:])
	case "capabilities":
		err = c.capabilities()
	case "relay-hook":
		ctx, cancel := context.WithTimeout(ctx, relayTimeout)
		defer cancel()
		return c.relayHook(ctx, event)
	case "ask-hook", "approval-hook", "stop-hook":
		ctx, cancel := context.WithTimeout(ctx, relayTimeout)
		defer cancel()
		switch rest[0] {
		case "ask-hook":
			return c.askHook(ctx, hook)
		case "approval-hook":
			fs := flag.NewFlagSet("approval-hook", flag.ContinueOnError)
			fs.SetOutput(stderr)
			wait := fs.Duration("wait", defaultApprovalWait, "how long to wait for an answer in Slack before the terminal asks")
			if fs.Parse(rest[1:]) != nil {
				return 0
			}
			return c.approvalHook(context.Background(), hook, *wait)
		}
		return c.stopHook(ctx, hook)
	default:
		err = fmt.Errorf("unknown command %q", rest[0])
	}
	if err != nil {
		fmt.Fprintf(stderr, "slack-mcp-server chat: %v\n", err)
		var ee exitError
		if errors.As(err, &ee) {
			return ee.code
		}
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
	cmd.Dir = c.home.StateDir // not the session's directory, which may be a worktree that goes away
	cmd.Env = listenerEnv(os.Environ())
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
	id, err := findChannel(ctx, c.bot, name)
	if err != nil {
		return "", "", fmt.Errorf("this agent's bot is %w", err)
	}
	return id, name, nil
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
		me, err := c.identity(ctx)
		if err != nil {
			return err
		}
		var ids []string
		for _, ch := range channels {
			id, name, err := c.resolveChannel(ctx, ch)
			if err != nil {
				return err
			}
			ids = append(ids, id)
			if _, derived := ProjectOf(name); !derived {
				// The project watch must not depend on the direct channel.
				if direct, err := c.ensureDirect(ctx, me, name); err != nil {
					fmt.Fprintf(c.stderr, "slack-agent-chat: no direct channel for #%s: %v\n", name, err)
				} else {
					ids = append(ids, direct)
				}
				// Side channels this agent was added to earlier (a session
				// that restarts would otherwise stop watching them).
				derived, err := derivedChannels(ctx, c.bot, name)
				if err != nil {
					fmt.Fprintf(c.stderr, "slack-agent-chat: listing #%s's side channels: %v\n", name, err)
				}
				ids = append(ids, derived...)
			}
		}
		return c.subscribe(ctx, uniq(ids), *backlog)
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

// lookupUsers maps names to users: agents (bots) when bots is true, else
// people, matched by username, display name or real name.
func (c *cli) lookupUsers(ctx context.Context, list string, bots bool) ([]slack.User, error) {
	var wanted []string
	for _, name := range strings.Split(list, ",") {
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
	kind := "person"
	if bots {
		kind = "agent"
	}
	var found []slack.User
	for _, name := range wanted {
		i := slices.IndexFunc(users, func(u slack.User) bool {
			return u.IsBot == bots && !u.Deleted &&
				(strings.EqualFold(u.Name, name) || strings.EqualFold(u.Profile.DisplayName, name) || strings.EqualFold(u.RealName, name))
		})
		if i < 0 {
			return nil, fmt.Errorf("no %s named %q", kind, name)
		}
		found = append(found, users[i])
	}
	return found, nil
}

func userIDs(users []slack.User) []string {
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	return ids
}

// identity is this home's owner (user token) and agent (bot token).
type identity struct {
	ownerID, ownerName string // the owner's username, used in channel names
	agentID, agentName string // the name Slack shows for the bot
}

func (c *cli) identity(ctx context.Context) (identity, error) {
	owner, err := c.user.AuthTestContext(ctx)
	if err != nil {
		return identity{}, fmt.Errorf("user auth.test: %w", err)
	}
	bot, err := c.bot.AuthTestContext(ctx)
	if err != nil {
		return identity{}, fmt.Errorf("bot auth.test: %w", err)
	}
	u, err := c.bot.GetUserInfoContext(ctx, bot.UserID)
	if err != nil {
		return identity{}, fmt.Errorf("users.info %s: %w", bot.UserID, err)
	}
	return identity{ownerID: owner.UserID, ownerName: owner.User, agentID: bot.UserID, agentName: shownName(u)}, nil
}

// ensureDirect makes sure #PROJECT__OWNER_AGENT exists with the owner in it
// and returns its ID.
func (c *cli) ensureDirect(ctx context.Context, me identity, project string) (string, error) {
	name, err := DirectChannelName(project, me.ownerName, me.agentName)
	if err != nil {
		return "", err
	}
	id, _, err := ensureChannel(ctx, c.bot, name, me.agentID, []string{me.ownerID})
	return id, err
}

func (c *cli) channel(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: channel create|invite")
	}
	fs := flag.NewFlagSet("channel", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	invite := fs.String("invite", "", "comma-separated agent names to invite")
	inviteUser := fs.String("invite-user", "", "comma-separated people to invite")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		if len(pos) != 1 {
			return errors.New("usage: channel create NAME [--invite a,b] [--invite-user u,v]")
		}
		name, err := NormalizeChannelName(pos[0])
		if err != nil {
			return err
		}
		if err := ValidateProjectName(name); err != nil {
			return err
		}
		agents, err := c.lookupUsers(ctx, *invite, true)
		if err != nil {
			return err
		}
		people, err := c.lookupUsers(ctx, *inviteUser, false)
		if err != nil {
			return err
		}
		me, err := c.identity(ctx)
		if err != nil {
			return err
		}
		// Created as the owner, so Slack records them as its creator: only
		// they (through any of their agents) may archive the project.
		members := append(append([]string{me.agentID}, userIDs(agents)...), userIDs(people)...)
		id, _, err := ensureChannel(ctx, c.user, name, me.ownerID, members)
		if err != nil {
			return err
		}
		// Created as the owner, so no bot is ever in it. Another person may
		// own it already; the project does not depend on it.
		usersID, _, err := ensureChannel(ctx, c.user, UsersChannelName(name), me.ownerID, userIDs(people))
		if err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: %v\n", err)
		}
		watch := []string{id}
		if direct, err := c.ensureDirect(ctx, me, name); err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: no direct channel for #%s: %v\n", name, err)
		} else {
			watch = append(watch, direct)
		}
		c.printJSON(map[string]any{"channel_id": id, "name": name, "users_channel_id": usersID})
		return c.subscribe(ctx, watch, 0)
	case "invite":
		if len(pos) < 1 || len(pos) > 2 {
			return errors.New("usage: channel invite CHANNEL [AGENT,AGENT] [--invite-user u,v]")
		}
		id, name, err := c.resolveChannel(ctx, pos[0])
		if err != nil {
			return err
		}
		agentList := ""
		if len(pos) == 2 {
			agentList = pos[1]
		}
		agents, err := c.lookupUsers(ctx, agentList, true)
		if err != nil {
			return err
		}
		people, err := c.lookupUsers(ctx, *inviteUser, false)
		if err != nil {
			return err
		}
		ids := append(userIDs(agents), userIDs(people)...)
		if len(ids) == 0 {
			return errors.New("name at least one agent or person to invite")
		}
		if err := inviteEach(ctx, c.bot, id, "", ids); err != nil {
			return fmt.Errorf("inviting to #%s: %w", name, err)
		}
		c.printJSON(map[string]any{"ok": true, "channel_id": id, "invited": ids})
		return nil
	}
	return fmt.Errorf("unknown channel command %q", args[0])
}

// side opens the side channel between this agent and others, or joins it if
// one of them already opened it, and watches it.
func (c *cli) side(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("side", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	channel := fs.String("channel", "", "the project channel (default: the one this session watches)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: side AGENT[,AGENT] [--channel PROJECT]")
	}
	agents, err := c.lookupUsers(ctx, pos[0], true)
	if err != nil {
		return err
	}
	var project string
	if *channel != "" {
		_, name, err := c.resolveChannel(ctx, *channel)
		if err != nil {
			return err
		}
		project, _ = ProjectOf(name)
	} else {
		sub, err := detectSession(os.Getenv)
		if err != nil {
			return err
		}
		if _, project, err = c.watchedProject(ctx, sub.SessionID); err != nil {
			return fmt.Errorf("%w; name it with --channel", err)
		}
	}
	me, err := c.identity(ctx)
	if err != nil {
		return err
	}
	names := []string{me.agentName}
	for _, a := range agents {
		names = append(names, shownName(&a))
	}
	name, err := SideChannelName(project, names)
	if err != nil {
		return err
	}
	id, created, err := ensureChannel(ctx, c.bot, name, me.agentID, append([]string{me.ownerID}, userIDs(agents)...))
	if err != nil {
		return err
	}
	c.printJSON(map[string]any{"channel_id": id, "name": name, "created": created})
	backlog := 0
	if !created {
		backlog = autoBacklog // joining: catch up on what is already there
	}
	return c.subscribe(ctx, []string{id}, backlog)
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

// markTurn records whether the prompt was typed at the terminal, for
// stop-hook. It never blocks the prompt: failures are only logged.
func markTurn(envFile string, event relayEvent, stderr io.Writer) {
	session := sessionOf(event.SessionID)
	path, err := ResolveEnvFile(envFile, os.Getenv)
	if err == nil {
		err = MarkTurn(NewHome(path), session, turnKey(event.PromptID, event.TurnID), event.Prompt)
	}
	if err != nil {
		fmt.Fprintf(stderr, "slack-agent-chat: marking turn: %v\n", err)
	}
}

func emitBlock(w io.Writer, err error) int {
	data, _ := json.Marshal(map[string]string{"decision": "block", "reason": "slack-agent-chat relay: " + err.Error()})
	fmt.Fprintln(w, string(data))
	return 0
}

// blockRelay blocks a %agents prompt. No turn runs, so its terminal-turn
// mark must not linger for a later turn's stop-hook.
func (c *cli) blockRelay(session string, err error) int {
	ClearTurn(c.home, session)
	return emitBlock(c.stdout, err)
}

// relayEvent is the part of a UserPromptSubmit hook event the relay reads.
type relayEvent struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
	PromptID  string `json:"prompt_id"` // Claude Code
	TurnID    string `json:"turn_id"`   // Codex
}

// relayHook posts a %agents prompt (already recognized by RunCLI) to Slack.
func (c *cli) relayHook(ctx context.Context, event relayEvent) int {
	target, text, _ := ParseRelayPrompt(event.Prompt)
	if text == "" {
		return c.blockRelay(sessionOf(event.SessionID), errors.New("nothing to relay after the %agents prefix"))
	}
	session := sessionOf(event.SessionID)
	channelID, channelName, err := c.relayTarget(ctx, session, target)
	if err != nil {
		return c.blockRelay(session, err)
	}
	// Announce the relay before posting so its echo cannot reach this session first.
	if _, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "expect", SessionID: session, Channel: channelID, Text: text}); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: could not register relay: %v\n", err)
	}
	ts, err := c.postAsOwner(ctx, channelID, "", text)
	if err != nil {
		return c.blockRelay(session, err)
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

// relayTarget picks the channel for a relay: the explicit target, or the
// project channel this session watches.
func (c *cli) relayTarget(ctx context.Context, session, target string) (string, string, error) {
	if target != "" {
		return c.resolveChannel(ctx, target)
	}
	id, name, err := c.watchedProject(ctx, session)
	if err != nil {
		return "", "", fmt.Errorf("%w; use %%agents@<channel>:", err)
	}
	return id, name, nil
}

// sessionChannels returns the channels session watches, by ID with names,
// using the names the listener reports and asking Slack for the rest.
func (c *cli) sessionChannels(ctx context.Context, session string) (ids []string, names map[string]string, err error) {
	resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "status"})
	if err != nil {
		return nil, nil, errors.New("no slack-agent-chat listener is running")
	}
	for _, s := range resp.Sessions {
		if s.SessionID != session {
			continue
		}
		names = map[string]string{}
		for _, ch := range s.Channels {
			name := s.Names[ch]
			if name == "" {
				if _, name, err = c.resolveChannel(ctx, ch); err != nil {
					return nil, nil, err
				}
			}
			names[ch] = name
		}
		return s.Channels, names, nil
	}
	return nil, nil, errors.New("this session is not watching a channel")
}

// watchedProject returns the one project channel (not a derived __ channel)
// that session watches.
func (c *cli) watchedProject(ctx context.Context, session string) (string, string, error) {
	ids, names, err := c.sessionChannels(ctx, session)
	if err != nil {
		return "", "", err
	}
	var projects []string
	for _, id := range ids {
		if _, derived := ProjectOf(names[id]); !derived {
			projects = append(projects, id)
		}
	}
	if len(projects) != 1 {
		return "", "", fmt.Errorf("this session watches %d project channels", len(projects))
	}
	return projects[0], names[projects[0]], nil
}

// listenerEnv returns the environment for a detached listener: only neutral
// variables, never the starting session's identity or secrets.
func listenerEnv(environ []string) []string {
	keep := map[string]bool{"HOME": true, "PATH": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "LANG": true, "CODEX_HOME": true}
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if keep[name] || strings.HasPrefix(name, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}
