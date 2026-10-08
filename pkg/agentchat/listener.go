package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/version"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const (
	maxRecovery       = 50
	reactionDelivered = "eyes"
	reactionAcked     = "white_check_mark"
	repeatWindow      = 10 * time.Minute
	relayExpiry       = 2 * time.Minute
	// autoBacklog is what a session gets from a channel it starts watching
	// because its bot was added, so it sees what was posted before.
	autoBacklog = 20
)

// SlackAPI is the part of *slack.Client the listener uses.
type SlackAPI interface {
	AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error)
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	AddReactionContext(ctx context.Context, name string, item slack.ItemRef) error
	PostMessageContext(ctx context.Context, channel string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channel, timestamp string, options ...slack.MsgOption) (string, string, string, error)
}

// UserAPI is the part of the owner's *slack.Client (user token) the listener
// uses to keep the project's people-only channel up to date.
type UserAPI interface {
	GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)
	InviteUsersToConversationContext(ctx context.Context, channelID string, users ...string) (*slack.Channel, error)
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
	// Users acts as the owner; nil leaves <project>__users alone.
	Users UserAPI

	mu            sync.Mutex
	state         *State
	repeats       *RepeatFilter
	users         map[string]*slack.User
	chNames       map[string]string
	relays        map[string]time.Time      // expected %agents echoes: session|channel|text → expiry
	sessLock      map[string]*sync.Mutex    // serializes deliveries per session
	queues        map[string]chan []pending // per-session delivery queues (Async)
	approvals     map[string]*approval      // approval-hook requests by approval ID
	views         cohortViews               // project views for cohort tracking
	approvalWaits map[string]*approvalWait  // Codex registrations seen waiting on an approval, by session|project
	unobservable  map[string]bool           // Codex sessions whose daemon hides approval waits (warned once)
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
		users: map[string]*slack.User{}, chNames: map[string]string{},
		relays: map[string]time.Time{}, sessLock: map[string]*sync.Mutex{}, queues: map[string]chan []pending{},
		approvals: map[string]*approval{},
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
	if u := l.user(ctx, id); u != nil {
		return shownName(u)
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

// notice builds m's notice. Every agent watching the channel gets every
// message except its own, as a person in the channel would see it.
func (l *Listener) notice(ctx context.Context, m Message) Notice {
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
	}
}

// HandleMessage routes one live message to every subscribed session.
func (l *Listener) HandleMessage(ctx context.Context, m Message) {
	if m.SubType == "channel_archive" || m.SubType == "group_archive" {
		l.dropChannel(m.Channel)
		return
	}
	if !m.Deliverable() {
		return
	}
	l.trackCohort(ctx, m) // before the self filter: this home's own GM answers count
	if m.From(l.Self) {
		return
	}
	if l.approvalReply(m) {
		l.markConsumed(m)
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
	n := l.notice(ctx, m)
	for _, sub := range watchers {
		if m.User != "" && m.User == l.OwnerID && l.consumeRelay(sub.SessionID, m) {
			continue
		}
		l.dispatch(ctx, sub, []pending{{m, n}})
	}
}

// HandleMemberJoined reacts to someone joining a channel this bot is in. When
// the bot itself is added to a channel derived from a project its sessions
// watch (a side channel another agent opened), those sessions start watching
// it. When a person joins a watched project channel, the owner adds them to
// <project>__users.
func (l *Listener) HandleMemberJoined(ctx context.Context, channel, user string) {
	if user == l.Self.UserID {
		l.autoWatch(ctx, channel)
		return
	}
	if !l.isAgent(ctx, user) {
		l.addToUsersChannel(ctx, channel, user)
	}
}

func (l *Listener) autoWatch(ctx context.Context, channel string) {
	project, derived := ProjectOf(l.channelName(ctx, channel))
	if !derived {
		return
	}
	l.mu.Lock()
	var subs []Subscription
	for _, sub := range l.state.Subscriptions {
		if !sub.Watches(channel) {
			cp := *sub
			cp.Channels = slices.Clone(sub.Channels) // Unsubscribe edits the stored slice in place
			subs = append(subs, cp)
		}
	}
	l.mu.Unlock()
	for _, sub := range subs {
		if !slices.ContainsFunc(sub.Channels, func(ch string) bool { return l.channelName(ctx, ch) == project }) {
			continue
		}
		sub.Channels = []string{channel}
		merged, first, err := l.register(&sub)
		if err != nil {
			l.Log.Warn("auto-watch failed", zap.String("session", sub.SessionID), zap.String("channel", channel), zap.Error(err))
			continue
		}
		l.Log.Info("auto-watching", zap.String("session", sub.SessionID), zap.String("channel", channel))
		l.recover(ctx, merged, sub.Channels, first, autoBacklog)
	}
}

func (l *Listener) addToUsersChannel(ctx context.Context, channel, user string) {
	if l.Users == nil {
		return
	}
	l.mu.Lock()
	watched := len(l.state.Watchers(channel)) > 0
	l.mu.Unlock()
	name := l.channelName(ctx, channel)
	if _, derived := ProjectOf(name); !watched || derived {
		return
	}
	target := UsersChannelName(name)
	id, err := findChannel(ctx, l.Users, target)
	if err != nil {
		l.Log.Info("not adding to people channel", zap.String("channel", target), zap.String("user", user), zap.Error(err))
		return
	}
	if err := inviteEach(ctx, l.Users, id, "", []string{user}); err != nil {
		l.Log.Warn("adding to people channel failed", zap.String("channel", target), zap.String("user", user), zap.Error(err))
		return
	}
	l.Log.Info("added to people channel", zap.String("channel", target), zap.String("user", user))
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
			items = append(items, pending{m, l.notice(ctx, m)})
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
			found = append(found, pending{m, l.notice(ctx, m)})
			if len(found) == n {
				break
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
	for k, r := range l.state.Cohort {
		if r.SessionID == sessionID && (channel == "" || r.Channel == channel || len(sub.Channels) == 0) {
			delete(l.state.Cohort, k)
		}
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
	case "approval-watch":
		l.WatchApproval(req.Approval, req.Channel, req.TS, req.Text)
	case "approval":
		decision, reason, known, ended := l.takeApproval(req.Approval)
		return ControlResponse{OK: true, Decision: decision, Text: reason, Unknown: !known, Ended: ended}
	case "approval-end":
		l.EndApproval(req.Approval, req.Text)
	case "cohort-register", "cohort-leave", "cohort-duty", "cohort-checkpoint", "cohort-status":
		return l.cohortControl(ctx, req)
	case "cohort-claim-check":
		return l.claimCheck(ctx, req)
	case "status":
		// Names save each hook a conversations.info call per channel.
		sessions := l.Status()
		for i := range sessions {
			sessions[i].Names = map[string]string{}
			for _, ch := range sessions[i].Channels {
				if name := l.channelName(ctx, ch); name != "" && name != ch {
					sessions[i].Names[ch] = name
				}
			}
		}
		return ControlResponse{OK: true, Sessions: sessions, Version: version.Version}
	default:
		return ControlResponse{Error: "unknown op " + req.Op}
	}
	return ControlResponse{OK: true, Sessions: l.Status()}
}

// approval is one approval-hook request the listener answers for.
type approval struct {
	channel, ts      string // the request message, once the hook registers it
	decision, reason string // "" until answered
	hint             bool   // the owner typed an allow word, which cannot approve
	early            *click // a click that arrived before the hook registered
	at               time.Time
	text             string    // the request as posted, to redraw it once it ends
	polled           time.Time // the hook's last poll; a hook that stops polling is gone
	ended            bool      // takes no more answers: the hook finished, or was given up on
	owed             string    // a redraw still owed (its outcome line) until one succeeds
	retryAt          time.Time // when to try an owed redraw again
	tries            int       // failed redraw attempts
}

// Owed redraws are retried with backoff from approvalRetry to approvalRetryMax,
// and given up after approvalRetries failures.
const (
	approvalRetry    = 5 * time.Second
	approvalRetryMax = 5 * time.Minute
	approvalRetries  = 10
)

// approvalAbandoned is how long a registered hook may go without polling
// (it polls every second) before its request counts as ended: the host
// killed it outright, or the session ended. A hook that finishes, however it
// finishes, says so (approval-end) after redrawing its own request.
const approvalAbandoned = 30 * time.Second

// click is an owner's button click on a message posted by this bot.
type click struct {
	channel, ts, decision string
}

// approvalEntry returns the request with id, creating it, and forgets
// requests older than an hour. Call with l.mu held.
func (l *Listener) approvalEntry(id string) *approval {
	for k, a := range l.approvals {
		if l.Now().Sub(a.at) > time.Hour {
			delete(l.approvals, k)
		}
	}
	a := l.approvals[id]
	if a == nil {
		a = &approval{at: l.Now()}
		l.approvals[id] = a
	}
	return a
}

// WatchApproval registers the message a hook posted for request id (text is
// its body), so replies in its thread are read as answers instead of
// delivered, and the message is redrawn if the hook goes away unanswered.
func (l *Listener) WatchApproval(id, channel, ts, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.approvalEntry(id)
	if a.ended {
		return // an ended request stays ended; a hook that re-registers it is told so
	}
	a.channel, a.ts, a.text, a.polled = channel, ts, text, l.Now()
	if c := a.early; c != nil && a.decision == "" && c.channel == channel && c.ts == ts {
		a.decision = c.decision
	}
	a.early = nil
}

// HandleInteraction records a click on an approval button. Slack vouches
// for who clicked, so a click is the only way to allow: agents can post as
// the owner, but cannot click as them. A click counts only when the owner
// made it, on the message this bot posted for that approval, with a known
// decision; the first such click wins.
func (l *Listener) HandleInteraction(payload []byte) {
	var in struct {
		Type string `json:"type"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Container struct {
			ChannelID string `json:"channel_id"`
			MessageTS string `json:"message_ts"`
		} `json:"container"`
		Message struct {
			BotID string `json:"bot_id"`
		} `json:"message"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if json.Unmarshal(payload, &in) != nil || in.Type != "block_actions" {
		return
	}
	for _, act := range in.Actions {
		decision, ok := strings.CutPrefix(act.ActionID, approvalActionPrefix)
		if !ok || act.Value == "" {
			continue
		}
		switch {
		case in.User.ID != l.OwnerID:
			l.Log.Warn("ignoring approval click from someone other than the owner", zap.String("user", in.User.ID), zap.String("approval", act.Value))
			continue
		case in.Message.BotID != l.Self.BotID:
			l.Log.Warn("ignoring approval click on a message this bot did not post", zap.String("bot", in.Message.BotID), zap.String("approval", act.Value))
			continue
		case decision != decisionAllow && decision != decisionDeny && decision != decisionTerminal:
			l.Log.Warn("ignoring approval click with an unknown decision", zap.String("decision", decision))
			continue
		}
		c := click{in.Container.ChannelID, in.Container.MessageTS, decision}
		l.mu.Lock()
		a := l.approvalEntry(act.Value)
		switch {
		case a.ended:
			l.Log.Info("ignoring a click on an ended approval request", zap.String("approval", act.Value))
		case a.decision != "":
		case a.channel == "":
			if a.early == nil {
				a.early = &c // checked when the hook registers its message
			}
		case a.channel == c.channel && a.ts == c.ts:
			a.decision = decision
		default:
			l.Log.Warn("ignoring approval click on a different message", zap.String("approval", act.Value), zap.String("channel", c.channel), zap.String("ts", c.ts))
		}
		l.mu.Unlock()
		l.Log.Info("approval clicked", zap.String("approval", act.Value), zap.String("decision", decision))
	}
}

// approvalReply consumes the owner's answer to a waiting approval request,
// so it is never also delivered as a notice:
//   - any reply in the request's thread (from anyone; only the owner's first
//     one that denies or picks the terminal answers it);
//   - a message in the channel itself, from the owner, that opens with a deny
//     word, "terminal" or an allow word, while a request there is unanswered
//     (the newest one, if several). Other messages are delivered as usual.
//
// A reply never allows: anything posting with the owner's token, an agent
// included, could have written it. An allow word earns a hint to click.
func (l *Listener) approvalReply(m Message) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	var target *approval
	explicit := true
	decision, reason := "", ""
	if m.ThreadTS != "" {
		for _, a := range l.approvals {
			if a.channel == m.Channel && a.ts == m.ThreadTS {
				target = a
				break
			}
		}
		if target == nil {
			return false
		}
		decision, reason = ParseApprovalReply(m.Text)
	} else {
		if m.User != l.OwnerID {
			return false
		}
		for _, a := range l.approvals {
			if a.channel == m.Channel && a.decision == "" && !a.ended && (target == nil || TSLess(target.ts, a.ts)) {
				target = a
			}
		}
		if target == nil {
			return false
		}
		if decision, reason, explicit = classifyApprovalReply(m.Text); !explicit {
			return false
		}
	}
	if m.User == l.OwnerID && target.decision == "" && !target.ended && explicit {
		if decision == decisionAllow {
			target.hint = true
		} else {
			target.decision, target.reason = decision, reason
		}
	}
	return true
}

// dropChannel stops every session watching an archived channel.
func (l *Listener) dropChannel(channel string) {
	l.mu.Lock()
	watchers := l.state.Watchers(channel)
	l.mu.Unlock()
	for _, sub := range watchers {
		l.Log.Info("channel archived; unwatching", zap.String("session", sub.SessionID), zap.String("channel", channel))
		l.Unsubscribe(sub.SessionID, channel)
	}
}

// markConsumed records m as delivered to every session watching its
// channel, so recovery after a listener restart never pushes an approval
// answer the listener kept out of the sessions.
func (l *Listener) markConsumed(m Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, sub := range l.state.Watchers(m.Channel) {
		l.state.MarkDelivered(sub.SessionID, m.Channel, m.TS, l.Now())
	}
	if err := l.state.Save(l.StateFile); err != nil {
		l.Log.Error("saving state failed", zap.Error(err))
	}
}

// TakeApproval returns request id's answer for the waiting hook: a decision
// with its reason, decisionHint once after the owner typed an allow word, or
// "" while unanswered.
func (l *Listener) TakeApproval(id string) (decision, reason string) {
	decision, reason, _, _ = l.takeApproval(id)
	return decision, reason
}

// takeApproval is TakeApproval that also reports whether the request is
// known (registered) to this listener, and whether it has ended: an ended
// request never hands out an answer, so a hook that resumes late learns it
// can decide nothing.
func (l *Listener) takeApproval(id string) (decision, reason string, known, ended bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.approvals[id]
	if a == nil {
		return "", "", false, false
	}
	a.polled = l.Now()
	known = a.channel != ""
	if a.ended {
		return "", "", known, true
	}
	switch {
	case a.decision == decisionTaken:
		return "", "", known, false
	case a.decision != "":
		decision, reason = a.decision, a.reason
		a.decision = decisionTaken // keep the entry so later replies stay out of the session
		return decision, reason, known, false
	case a.hint:
		a.hint = false
		return decisionHint, "", known, false
	}
	return "", "", known, false
}

// EndApproval records that request id's hook finished. owed is the outcome
// line when the hook's own redraw failed, for the sweep to retry; "" when it
// succeeded.
func (l *Listener) EndApproval(id, owed string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.approvals[id]; a != nil {
		a.ended, a.owed, a.retryAt, a.tries = true, owed, l.Now(), 0
	}
}

// SweepApprovals ends each registered request whose hook stopped polling
// without saying it finished (approval-end), and redraws it, so Slack never
// shows live buttons for a prompt that ended elsewhere: the terminal answered
// it, or the hook was killed outright. It also retries redraws still owed
// after a failure, with backoff.
func (l *Listener) SweepApprovals(ctx context.Context) {
	type redraw struct{ id, channel, ts, text, line string }
	var todo []redraw
	l.mu.Lock()
	now := l.Now()
	for id, a := range l.approvals {
		if a.channel == "" {
			continue
		}
		if !a.ended && now.Sub(a.polled) >= approvalAbandoned {
			a.ended, a.retryAt, a.tries = true, now, 0
			a.owed = "↩️ No longer waiting: it was answered in the terminal, or the request ended. Nothing was decided here."
			switch a.decision {
			case decisionTaken: // the hook took the answer but never finished redrawing
				a.owed = "↩️ The request ended; its outcome is in the session."
			case "":
			default:
				a.owed = "↩️ Your answer arrived after the request had ended, so it changed nothing."
			}
		}
		if a.owed != "" && !now.Before(a.retryAt) {
			todo = append(todo, redraw{id, a.channel, a.ts, a.text, a.owed})
		}
	}
	l.mu.Unlock()
	for _, r := range todo {
		blocks := []slack.Block{slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, r.line, false, false))}
		if r.text != "" {
			blocks = append([]slack.Block{slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, r.text, false, false), nil, nil)}, blocks...)
		}
		_, _, _, err := l.API.UpdateMessageContext(ctx, r.channel, r.ts, slack.MsgOptionText(r.line, false), slack.MsgOptionBlocks(blocks...))
		l.mu.Lock()
		if a := l.approvals[r.id]; a != nil && a.owed == r.line {
			switch {
			case err == nil:
				a.owed = ""
			case a.tries+1 >= approvalRetries:
				a.owed = ""
				l.Log.Error("giving up redrawing an ended approval request", zap.String("channel", r.channel), zap.String("ts", r.ts), zap.Error(err))
			default:
				a.tries++
				a.retryAt = l.Now().Add(min(approvalRetry<<a.tries, approvalRetryMax))
				l.Log.Warn("redrawing an ended approval request failed; will retry", zap.String("channel", r.channel), zap.String("ts", r.ts), zap.Error(err))
			}
		}
		l.mu.Unlock()
	}
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
