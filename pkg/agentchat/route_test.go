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

func TestMentionsTokens(t *testing.T) {
	got := Mentions("<@UCR> and <@UBRIAN> please", resolveUser)
	assert.Equal(t, []string{"UCR", "UBRIAN"}, got)
}

func TestMentionsPlainNames(t *testing.T) {
	got := Mentions("@codex-r, can you check? cc @claude. thanks @brian", resolveUser)
	assert.ElementsMatch(t, []string{"UCR", "UCL", "UBR"}, got)
	assert.Empty(t, Mentions("mail me at x@codex-r.com", resolveUser))
	assert.Empty(t, Mentions("@nobody-known here", resolveUser))
}

func TestShouldDeliver(t *testing.T) {
	self := Identity{UserID: "UCL", BotID: "BCL"}
	m := Message{User: "UCB", Text: "x"}
	assert.True(t, ShouldDeliver(m, self, nil), "no mentions: broadcast to all agents")
	assert.False(t, ShouldDeliver(Message{User: "UCL", BotID: "BCL"}, self, nil), "own message")
	assert.True(t, ShouldDeliver(m, self, []string{"UCL"}), "addressed to me")
	assert.False(t, ShouldDeliver(m, self, []string{"UCR"}), "addressed to another agent")
	assert.False(t, ShouldDeliver(m, self, []string{"UBR"}), "addressed only to a person")
	assert.True(t, ShouldDeliver(m, self, []string{"UBR", "UCL"}), "person and me")
	assert.False(t, ShouldDeliver(m, self, []string{"UBR", "UCR"}), "person and another agent")
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
