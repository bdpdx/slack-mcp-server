package agentchat

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	replyPage int                        // when > 0, replies are served this many per page
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
	all := f.replies[p.ChannelID+"|"+p.Timestamp]
	if f.replyPage <= 0 {
		return all, false, "", nil
	}
	start := 0
	if p.Cursor != "" {
		fmt.Sscanf(p.Cursor, "%d", &start)
	}
	end := min(start+f.replyPage, len(all))
	next := ""
	if end < len(all) {
		next = fmt.Sprint(end)
	}
	return all[start:end], next != "", next, nil
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
	dead map[string]bool  // sessions Alive reports as gone
}

func (d *fakeDeliverer) Deliver(_ context.Context, sub *Subscription, clientID, text string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[sub.SessionID]; err != nil {
		return "", err
	}
	d.got = append(d.got, delivery{sub.SessionID, clientID, text})
	return "fake", nil
}

func (d *fakeDeliverer) Alive(_ context.Context, sub *Subscription) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.dead[sub.SessionID], nil
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

	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.000001", User: "UCB", Text: "<@UCR> just hi"})
	require.Len(t, d.got, 0, "opens with codex-r only")

	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.000002", User: "UBR", Text: "status please; <@UCR> rerun the tests"})
	require.Len(t, d.got, 1, "a later mention does not narrow delivery")
	assert.Contains(t, d.got[0].text, "#proj (C1) from brian (the console user")
	assert.Contains(t, d.got[0].text, "@codex-r rerun the tests")
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

// Bots usually have no display name; Slack shows their real name, so notices must too.
func TestListenerSenderNamePrefersRealNameOverUsername(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	api.users["UCB"] = &slack.User{ID: "UCB", Name: "codexb", RealName: "codex-b", IsBot: true}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UCB", Text: "hi"})
	require.Len(t, d.got, 1)
	assert.Contains(t, d.got[0].text, "from codex-b,")
}

// SAC-11: an agent's reply addressed only to a person must not wake other agents.
func TestListenerReplyToPersonOnlyReachesNoAgents(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UCB", Text: "@brian done, tests pass"})
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.2", User: "UCB", Text: "<@UBR> done"})
	assert.Empty(t, d.got)
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.3", User: "UCB", Text: "@brian @claude FYI"})
	assert.Len(t, d.got, 1, "a person plus this agent still reaches this agent")
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

func TestListenerExpectedRelayNeverEchoesToSender(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))

	// Registered before the post, so the echo cannot win the race.
	l.ExpectRelay("s1", "C1", "ship a & b ")
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.5", User: "UBR", Text: "ship a &amp; b"})
	require.Len(t, d.got, 1)
	assert.Equal(t, "s2", d.got[0].session)

	// The expectation is consumed: the same words typed later in Slack arrive normally.
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.6", User: "UBR", Text: "ship a &amp; b"})
	assert.Len(t, d.got, 3)
}

// blockingDeliverer holds its first delivery until released.
type blockingDeliverer struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (b *blockingDeliverer) Alive(context.Context, *Subscription) (bool, error) { return true, nil }

func (b *blockingDeliverer) Deliver(context.Context, *Subscription, string, string) (string, error) {
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.mu.Unlock()
	if n == 1 {
		close(b.entered)
		<-b.release
	}
	return "fake", nil
}

func TestListenerConcurrentDeliveriesOfOneMessagePushOnce(t *testing.T) {
	b := &blockingDeliverer{entered: make(chan struct{}), release: make(chan struct{})}
	l, err := NewListener(newFakeSlack(), b, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	m := Message{Channel: "C1", TS: "2001.1", User: "UBR", Text: "x"}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.HandleMessage(context.Background(), m) }()
	<-b.entered
	go func() { defer wg.Done(); l.HandleMessage(context.Background(), m) }() // e.g. recovery racing the live event
	time.Sleep(50 * time.Millisecond)
	close(b.release)
	wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()
	assert.Equal(t, 1, b.calls)
}

// gatedDeliverer records deliveries; sessions in hold wait until released.
type gatedDeliverer struct {
	mu   sync.Mutex
	got  []delivery
	hold map[string]chan struct{}
}

func (g *gatedDeliverer) Alive(context.Context, *Subscription) (bool, error) { return true, nil }

func (g *gatedDeliverer) Deliver(_ context.Context, sub *Subscription, clientID, text string) (string, error) {
	if gate := g.hold[sub.SessionID]; gate != nil {
		<-gate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.got = append(g.got, delivery{sub.SessionID, clientID, text})
	return "fake", nil
}

func (g *gatedDeliverer) sessions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, d := range g.got {
		out = append(out, d.session)
	}
	return out
}

// SAC-9: a hung session must not hold up the Slack event loop or other sessions.
func TestListenerAsyncHungSessionDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	g := &gatedDeliverer{hold: map[string]chan struct{}{"s1": release}}
	l, err := NewListener(newFakeSlack(), g, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", filepath.Join(t.TempDir(), "state.json"), zap.NewNop())
	require.NoError(t, err)
	l.Async = true
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))

	start := time.Now()
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UBR", Text: "one"})
	l.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.2", User: "UBR", Text: "two"})
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the event loop must not wait for deliveries")
	assert.Eventually(t, func() bool { return len(g.sessions()) == 2 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"s2", "s2"}, g.sessions(), "s2 is served while s1 hangs")

	close(release)
	assert.Eventually(t, func() bool { return len(g.sessions()) == 4 }, 2*time.Second, 10*time.Millisecond)
	g.mu.Lock()
	var s1 []string
	for _, d := range g.got {
		if d.session == "s1" {
			s1 = append(s1, d.text)
		}
	}
	g.mu.Unlock()
	require.Len(t, s1, 2)
	assert.Contains(t, s1[0], "one", "each session's messages keep their order")
	assert.Contains(t, s1[1], "two")
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
	assert.Less(t, strings.Index(d.got[0].text, "alpha"), strings.Index(d.got[0].text, "gamma"))
	assert.NotContains(t, d.got[0].text, "mine-own")
}

// SAC-8: --backlog N means N messages this agent would have received, not
// the last N raw messages.
func TestListenerBacklogCountsRoutedMessages(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	api.history["C1"] = []slack.Message{ // newest first
		msg("1999.5", "UBR", "@codex-r a"),
		msg("1999.4", "UBR", "@codex-r b"),
		msg("1999.3", "UBR", "mine-one"),
		msg("1999.2", "UBR", "@codex-r c"),
		msg("1999.1", "UBR", "mine-two"),
	}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 2))
	require.Len(t, d.got, 1)
	assert.Contains(t, d.got[0].text, "2 pending messages")
	assert.Contains(t, d.got[0].text, "mine-one")
	assert.Contains(t, d.got[0].text, "mine-two")
	assert.NotContains(t, d.got[0].text, "@codex-r")
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

// SAC-10: sessions that ended without `watch stop` are dropped even when no
// message arrives, so the listener can idle-exit.
func TestListenerSweepDropsGoneSessions(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{dead: map[string]bool{"s1": true}}
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0))
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	l.Sweep(context.Background())
	st := l.Status()
	require.Len(t, st, 1)
	assert.Equal(t, "s2", st[0].SessionID)
}

// SAC-7: catch-up must read every page of a long thread's replies.
func TestListenerRecoveryReadsAllReplyPages(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	api.replyPage = 2
	l := newTestListener(t, api, d)
	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s1"), 0)) // join point = 2000.000000

	parent := msg("2001.1", "UCB", "parent")
	parent.ReplyCount, parent.LatestReply = 4, "2001.5"
	api.history["C1"] = []slack.Message{parent}
	thread := []slack.Message{parent}
	for i, text := range []string{"r-one", "r-two", "r-three", "r-four"} {
		r := msg(fmt.Sprintf("2001.%d", i+2), "UBR", text)
		r.ThreadTimestamp = "2001.1"
		thread = append(thread, r)
	}
	api.replies["C1|2001.1"] = thread

	require.NoError(t, l.Subscribe(context.Background(), claudeSub("s2"), 0))
	require.Len(t, d.got, 1)
	for _, text := range []string{"r-one", "r-two", "r-three", "r-four"} {
		assert.Contains(t, d.got[0].text, text)
	}
}

// SAC-6: after a listener restart, every restored session catches up on
// what arrived while the listener was down, without repeats.
func TestListenerRecoverAllAfterRestart(t *testing.T) {
	api, d := newFakeSlack(), &fakeDeliverer{}
	state := filepath.Join(t.TempDir(), "state.json")
	first, err := NewListener(api, d, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", state, zap.NewNop())
	require.NoError(t, err)
	first.Now = func() time.Time { return time.Unix(2000, 0) }
	require.NoError(t, first.Subscribe(context.Background(), claudeSub("s1"), 0))
	require.NoError(t, first.Subscribe(context.Background(), claudeSub("s2"), 0))
	first.HandleMessage(context.Background(), Message{Channel: "C1", TS: "2001.1", User: "UBR", Text: "before-restart"})
	require.Len(t, d.got, 2)

	// The listener stops; one more message arrives while it is down.
	api.history["C1"] = []slack.Message{msg("2002.1", "UBR", "while-down"), msg("2001.1", "UBR", "before-restart")}
	d.got = nil

	restarted, err := NewListener(api, d, Identity{UserID: "UCL", BotID: "BCL"}, "UBR", state, zap.NewNop())
	require.NoError(t, err)
	restarted.Now = first.Now // same clock, so the ledger's retention pruning keeps the earlier deliveries
	restarted.RecoverAll(context.Background())
	require.Len(t, d.got, 2)
	for _, got := range d.got {
		assert.Contains(t, got.text, "while-down")
		assert.NotContains(t, got.text, "before-restart")
	}
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
