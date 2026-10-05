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

var idMention = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)

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
