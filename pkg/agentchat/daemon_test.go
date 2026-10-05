package agentchat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseEventsAPIMessage(t *testing.T) {
	payload := []byte(`{"type":"event_callback","event":{"type":"message","channel":"C1","user":"UBR","text":"hi","ts":"2.0","thread_ts":"1.0","files":[{"name":"a.png"}]}}`)
	m, ok := ParseEventsAPIMessage(payload)
	assert.True(t, ok)
	assert.Equal(t, Message{Channel: "C1", TS: "2.0", ThreadTS: "1.0", User: "UBR", Text: "hi", Files: []string{"a.png"}}, m)

	m, ok = ParseEventsAPIMessage([]byte(`{"event":{"type":"message","subtype":"bot_message","channel":"C1","bot_id":"B1","text":"x","ts":"3.0"}}`))
	assert.True(t, ok)
	assert.Equal(t, "B1", m.BotID)
	assert.Equal(t, "bot_message", m.SubType)

	_, ok = ParseEventsAPIMessage([]byte(`{"event":{"type":"reaction_added"}}`))
	assert.False(t, ok)
	_, ok = ParseEventsAPIMessage([]byte(`not json`))
	assert.False(t, ok)
}
