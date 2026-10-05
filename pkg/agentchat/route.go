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
