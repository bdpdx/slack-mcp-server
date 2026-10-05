package agentchat

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var agents = map[string]string{"UCB": "codex-b", "UCR": "codex-r", "UCL": "claude"}

func isAgent(id string) bool { _, ok := agents[id]; return ok }
func resolveAgent(name string) string {
	for id, n := range agents {
		if n == name {
			return id
		}
	}
	return ""
}

func TestAgentMentionsTokens(t *testing.T) {
	got := AgentMentions("<@UCR> and <@UBRIAN> please", isAgent, resolveAgent)
	assert.Equal(t, []string{"UCR"}, got)
}

func TestAgentMentionsPlainName(t *testing.T) {
	got := AgentMentions("@codex-r, can you check? cc @claude.", isAgent, resolveAgent)
	assert.ElementsMatch(t, []string{"UCR", "UCL"}, got)
	assert.Empty(t, AgentMentions("mail me at x@codex-r.com", isAgent, resolveAgent))
	assert.Empty(t, AgentMentions("@brian thoughts?", isAgent, resolveAgent))
}

func TestShouldDeliver(t *testing.T) {
	self := Identity{UserID: "UCL", BotID: "BCL"}
	broadcast := Message{User: "UCB", Text: "hi all"}
	assert.True(t, ShouldDeliver(broadcast, self, nil))
	assert.False(t, ShouldDeliver(Message{User: "UCL", BotID: "BCL"}, self, nil), "own message")
	assert.True(t, ShouldDeliver(broadcast, self, []string{"UCL"}))
	assert.False(t, ShouldDeliver(broadcast, self, []string{"UCR"}))
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
