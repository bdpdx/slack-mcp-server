package agentchat

import (
	"context"
	"errors"
	"fmt"
	"html"
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
	relayExpiry       = 2 * time.Minute
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
	// Async hands each session's deliveries to its own worker so a slow
	// session never holds up the caller. The daemon enables it.
	Async bool

	mu       sync.Mutex
	state    *State
	repeats  *RepeatFilter
	users    map[string]*slack.User
	loadedAt map[string]time.Time
	chNames  map[string]string
	relays   map[string]time.Time      // expected %agents echoes: session|channel|text → expiry
	sessLock map[string]*sync.Mutex    // serializes deliveries per session
	queues   map[string]chan []pending // per-session delivery queues (Async)
}

// queueDepth bounds each session's pending deliveries. Overflow is dropped;
// the messages lack this agent's ✅, so the next watch start catches them up.
const queueDepth = 100

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
		relays: map[string]time.Time{}, sessLock: map[string]*sync.Mutex{}, queues: map[string]chan []pending{},
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
	case u.RealName != "":
		// What Slack shows for users (and bots) without a display name.
		return u.RealName
	}
	return u.Name
}

// loadMembers caches the channel's members so plain @names resolve.
func (l *Listener) loadMembers(ctx context.Context, channel string) {
	l.mu.Lock()
	at, loaded := l.loadedAt[channel]
	fresh := loaded && l.Now().Sub(at) < memberRefresh
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

// resolve maps a plain @name to a known user, preferring an agent when an
// agent and a person share the name.
func (l *Listener) resolve(name string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	person := ""
	for id, u := range l.users {
		if strings.EqualFold(u.Name, name) || strings.EqualFold(u.Profile.DisplayName, name) || strings.EqualFold(u.RealName, name) {
			if u.IsBot {
				return id
			}
			person = id
		}
	}
	return person
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
	if !ShouldDeliver(m, l.Self, LeadingMentions(m.Text, l.resolve)) {
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
		if m.User != "" && m.User == l.OwnerID && l.consumeRelay(sub.SessionID, m) {
			continue
		}
		l.dispatch(ctx, sub, []pending{{m, n}})
	}
}

// dispatch delivers now, or with Async queues the delivery on the session's
// own worker so a slow or hung session never holds up the caller.
func (l *Listener) dispatch(ctx context.Context, sub *Subscription, items []pending) {
	if !l.Async {
		l.deliverTo(ctx, sub, items)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.queues[sub.SessionID]
	if q == nil {
		q = make(chan []pending, queueDepth)
		l.queues[sub.SessionID] = q
		go l.work(ctx, sub.SessionID, q)
	}
	select {
	case q <- items:
	default:
		l.Log.Warn("delivery queue full; message left for catch-up", zap.String("session", sub.SessionID))
	}
}

// work delivers one session's queued items in order until the queue is
// closed, using the session's current subscription and skipping items left
// after the session was dropped.
func (l *Listener) work(ctx context.Context, sessionID string, q chan []pending) {
	for items := range q {
		l.mu.Lock()
		sub := l.state.Subscriptions[sessionID]
		l.mu.Unlock()
		if sub != nil {
			l.deliverTo(ctx, sub, items)
		}
	}
}

func relayKey(sessionID, channel, text string) string {
	return sessionID + "|" + channel + "|" + strings.TrimSpace(html.UnescapeString(text))
}

// ExpectRelay records that sessionID is about to post text to channel as the
// owner (an %agents relay), so its echo is not delivered back to that session.
func (l *Listener) ExpectRelay(sessionID, channel, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.relays[relayKey(sessionID, channel, text)] = l.Now().Add(relayExpiry)
}

// consumeRelay reports whether m is the echo of a relay sessionID sent, and
// if so records it as delivered to that session.
func (l *Listener) consumeRelay(sessionID string, m Message) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	for k, exp := range l.relays {
		if now.After(exp) {
			delete(l.relays, k)
		}
	}
	key := relayKey(sessionID, m.Channel, m.Text)
	if _, ok := l.relays[key]; !ok {
		return false
	}
	delete(l.relays, key)
	l.state.MarkDelivered(sessionID, m.Channel, m.TS, now)
	if err := l.state.Save(l.StateFile); err != nil {
		l.Log.Error("saving state failed", zap.Error(err))
	}
	return true
}

func (l *Listener) sessionLock(sessionID string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.sessLock[sessionID]
	if m == nil {
		m = &sync.Mutex{}
		l.sessLock[sessionID] = m
	}
	return m
}

// deliverTo pushes the items sub hasn't had yet as one notice, records them
// and marks each one delivered with a reaction.
func (l *Listener) deliverTo(ctx context.Context, sub *Subscription, items []pending) {
	// One delivery per session at a time, so live events and recovery cannot
	// both push the same message.
	sl := l.sessionLock(sub.SessionID)
	sl.Lock()
	defer sl.Unlock()
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
	method, err := l.Deliverer.Deliver(ctx, sub, clientMessageID(sub.SessionID, last.Channel, last.TS), text)
	if err != nil {
		if errors.Is(err, ErrSessionGone) {
			l.Log.Info("session gone; dropping its subscription", zap.String("session", sub.SessionID), zap.String("kind", sub.Kind), zap.Error(err))
			l.Unsubscribe(sub.SessionID, "")
			return
		}
		l.Log.Warn("delivery failed", zap.String("session", sub.SessionID), zap.String("kind", sub.Kind), zap.Error(err))
		return
	}
	l.Log.Info("delivered", zap.String("session", sub.SessionID), zap.String("kind", sub.Kind), zap.String("method", method), zap.Int("messages", len(fresh)))
	l.mu.Lock()
	for _, it := range fresh {
		l.state.MarkDelivered(sub.SessionID, it.msg.Channel, it.msg.TS, l.Now())
	}
	l.state.Prune(l.Now())
	err = l.state.Save(l.StateFile)
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
		if first[ch] {
			if backlog > 0 {
				found, err := l.routedBacklog(ctx, ch, backlog)
				if err != nil {
					l.Log.Warn("reading history failed", zap.String("channel", ch), zap.Error(err))
				}
				items = append(items, found...)
			}
			continue
		}
		l.mu.Lock()
		join := l.state.JoinTS[ch]
		l.mu.Unlock()
		msgs, err := l.pendingSince(ctx, ch, join)
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

// routedBacklog returns up to n of the channel's most recent messages that
// this agent would have received, oldest first, paging back through at most
// 1000 messages of history.
func (l *Listener) routedBacklog(ctx context.Context, channel string, n int) ([]pending, error) {
	var found []pending
	cursor := ""
	for page := 0; page < 5 && len(found) < n; page++ {
		resp, err := l.API.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: channel, Cursor: cursor, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, sm := range resp.Messages { // newest first
			m := toMessage(channel, sm)
			if !m.Deliverable() || m.From(l.Self) {
				continue
			}
			if notice, ok := l.prepare(ctx, m); ok {
				found = append(found, pending{m, notice})
				if len(found) == n {
					break
				}
			}
		}
		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}
	slices.Reverse(found)
	return found, nil
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
				replyCursor := ""
				for replyPage := 0; replyPage < 20; replyPage++ {
					replies, _, next, err := l.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channel, Timestamp: m.Timestamp, Oldest: join, Cursor: replyCursor, Limit: 200})
					if err != nil {
						return nil, err
					}
					for _, r := range replies {
						if r.Timestamp != m.Timestamp && TSLess(join, r.Timestamp) {
							keep(r)
						}
					}
					if next == "" {
						break
					}
					replyCursor = next
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
		if q := l.queues[sessionID]; q != nil {
			close(q)
			delete(l.queues, sessionID)
		}
	}
	if err := l.state.Save(l.StateFile); err != nil {
		l.Log.Error("saving state failed", zap.Error(err))
	}
}

// Sweep drops subscriptions whose sessions have ended, so a session that quit
// without `watch stop` stops receiving and the listener can idle-exit. A
// session whose liveness cannot be determined is kept.
func (l *Listener) Sweep(ctx context.Context) {
	l.mu.Lock()
	subs := make([]*Subscription, 0, len(l.state.Subscriptions))
	for _, sub := range l.state.Subscriptions {
		subs = append(subs, sub)
	}
	l.mu.Unlock()
	for _, sub := range subs {
		alive, err := l.Deliverer.Alive(ctx, sub)
		if err != nil {
			l.Log.Warn("liveness check failed", zap.String("session", sub.SessionID), zap.Error(err))
			continue
		}
		if !alive {
			l.Log.Info("session gone; dropping its subscription", zap.String("session", sub.SessionID), zap.String("kind", sub.Kind))
			l.Unsubscribe(sub.SessionID, "")
		}
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
	case "expect":
		l.ExpectRelay(req.SessionID, req.Channel, req.Text)
	case "status":
	default:
		return ControlResponse{Error: "unknown op " + req.Op}
	}
	return ControlResponse{OK: true, Sessions: l.Status()}
}

// RecoverAll catches every restored session up on messages that arrived while
// the listener was down; messages already delivered to a session are skipped.
func (l *Listener) RecoverAll(ctx context.Context) {
	l.mu.Lock()
	subs := make([]*Subscription, 0, len(l.state.Subscriptions))
	for _, sub := range l.state.Subscriptions {
		subs = append(subs, sub)
	}
	l.mu.Unlock()
	for _, sub := range subs {
		l.recover(ctx, sub, sub.Channels, nil, 0)
	}
}
