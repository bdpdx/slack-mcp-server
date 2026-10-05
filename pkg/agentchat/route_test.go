package agentchat

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var agents = map[string]string{"UCB": "codex-b", "UCR": "codex-r", "UCL": "claude"}
var people = map[string]string{"UBR": "brian"}

// resolveUser maps a plain @name to any known user, agent or person.
func resolveUser(name string) string {
	for _, m := range []map[string]string{agents, people} {
		for id, n := range m {
			if n == name {
				return id
			}
		}
	}
	return ""
}

func TestLeadingMentionsTokens(t *testing.T) {
	got, addressed := LeadingMentions("<@UCR> and <@UBRIAN|brian>: please", resolveUser)
	assert.True(t, addressed)
	assert.Equal(t, []string{"UCR", "UBRIAN"}, got)
}

func TestLeadingMentionsPlainNames(t *testing.T) {
	got, addressed := LeadingMentions("  @codex-r, @claude & @brian: can you check?", resolveUser)
	assert.True(t, addressed)
	assert.Equal(t, []string{"UCR", "UCL", "UBR"}, got)
	got, _ = LeadingMentions("@claude. thanks @brian", resolveUser)
	assert.Equal(t, []string{"UCL"}, got)
	got, addressed = LeadingMentions("mail me at x@codex-r.com", resolveUser)
	assert.False(t, addressed)
	assert.Empty(t, got)
}

func TestLeadingMentionsUnknownNames(t *testing.T) {
	got, addressed := LeadingMentions("@nobody-known here", resolveUser)
	assert.True(t, addressed, "an unknown leading name still addresses the message")
	assert.Empty(t, got)
	got, addressed = LeadingMentions("@nobody-known @claude go", resolveUser)
	assert.True(t, addressed)
	assert.Equal(t, []string{"UCL"}, got, "an unknown name does not end the run")
}

func TestLeadingMentionsIgnoresLaterMentions(t *testing.T) {
	for _, text := range []string{
		"everyone pull main; @codex-r then rerun the tests",
		"heads up <@UCR> and @claude",
		"and @claude go",
	} {
		got, addressed := LeadingMentions(text, resolveUser)
		assert.False(t, addressed, text)
		assert.Empty(t, got, text)
	}
}

func TestShouldDeliver(t *testing.T) {
	self := Identity{UserID: "UCL", BotID: "BCL"}
	m := Message{User: "UCB", Text: "x"}
	assert.True(t, ShouldDeliver(m, self, false, nil), "not addressed: broadcast to all agents")
	assert.False(t, ShouldDeliver(Message{User: "UCL", BotID: "BCL"}, self, false, nil), "own message")
	assert.False(t, ShouldDeliver(m, self, true, nil), "addressed only to unknown names")
	assert.True(t, ShouldDeliver(m, self, true, []string{"UCL"}), "addressed to me")
	assert.False(t, ShouldDeliver(m, self, true, []string{"UCR"}), "addressed to another agent")
	assert.False(t, ShouldDeliver(m, self, true, []string{"UBR"}), "addressed only to a person")
	assert.True(t, ShouldDeliver(m, self, true, []string{"UBR", "UCL"}), "person and me")
	assert.False(t, ShouldDeliver(m, self, true, []string{"UBR", "UCR"}), "person and another agent")
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
