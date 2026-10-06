package agentchat

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

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
