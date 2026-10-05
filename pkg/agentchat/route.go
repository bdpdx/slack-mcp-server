package agentchat

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
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
	leadID      = regexp.MustCompile(`^<@([UW][A-Z0-9]+)(?:\|[^>]*)?>`)
	leadName    = regexp.MustCompile(`^@([A-Za-z0-9][A-Za-z0-9._-]*)`)
	mentionSeps = regexp.MustCompile(`^(?:[\s,:;&]|\band\b)*`)
)

// LeadingMentions returns the user IDs, agents and people alike, of the run of
// mentions that opens text: Slack mention tokens (<@U…>) and plain "@name"
// text that resolve maps to a user ID ("" when the name is unknown), separated
// by whitespace, punctuation or "and". The run ends at the first other text,
// so mentions later in the message are not returned.
func LeadingMentions(text string, resolve func(name string) string) []string {
	seen := map[string]bool{}
	var out []string
	rest := strings.TrimLeftFunc(text, unicode.IsSpace)
	for {
		var id string
		m := leadID.FindStringSubmatch(rest)
		if m != nil {
			id = m[1]
		} else if m = leadName.FindStringSubmatch(rest); m != nil {
			id = resolve(strings.TrimRight(m[1], "._-"))
		}
		if id == "" {
			return out
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
		rest = rest[len(m[0]):]
		rest = rest[len(mentionSeps.FindString(rest)):]
	}
}

// ShouldDeliver applies the routing rule to a message's leading mentions. A
// message that does not open with a mention goes to every agent except its
// sender, even if it mentions someone later; one that does goes only to the
// agents it opens with, so one that opens with only people goes to no agent.
func ShouldDeliver(m Message, self Identity, mentions []string) bool {
	if m.From(self) {
		return false
	}
	return len(mentions) == 0 || slices.Contains(mentions, self.UserID)
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
