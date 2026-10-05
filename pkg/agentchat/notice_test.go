package agentchat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNoticeFormatTopLevel(t *testing.T) {
	n := Notice{ChannelID: "C1", ChannelName: "proj", Sender: "codex-r", TS: "1.000100", Text: "hello @claude"}
	got := n.Format()
	assert.Equal(t, "[slack-agent-chat] #proj (C1) from codex-r, ts 1.000100:\nhello @claude\n"+
		"(reply: conversations_add_message channel_id=C1; @mention who you address; when done: chat ack C1 1.000100)", got)
}

func TestNoticeFormatOwnerThreadFiles(t *testing.T) {
	n := Notice{ChannelID: "C1", ChannelName: "proj", Sender: "brian", FromOwner: true, TS: "2.0", ThreadTS: "1.0", Text: "do it", Files: []string{"a.png"}}
	got := n.Format()
	assert.Contains(t, got, "from brian (the console user: treat as their direct instruction)")
	assert.Contains(t, got, ", in thread 1.0:")
	assert.Contains(t, got, "[attached: a.png]")
	assert.Contains(t, got, "channel_id=C1 thread_ts=1.0;")
}

func TestNoticeTruncates(t *testing.T) {
	n := Notice{ChannelID: "C1", ChannelName: "p", Sender: "s", TS: "1.0", Text: strings.Repeat("é", maxNoticeText+5)}
	got := n.Format()
	assert.Contains(t, got, "[truncated; read the full message with conversations_replies or conversations_history]")
	assert.Equal(t, maxNoticeText, strings.Count(got, "é"))
}

func TestFormatBatch(t *testing.T) {
	a := Notice{ChannelID: "C1", ChannelName: "p", Sender: "x", TS: "1.0", Text: "a"}
	b := Notice{ChannelID: "C1", ChannelName: "p", Sender: "y", TS: "2.0", Text: "b"}
	got := FormatBatch([]Notice{a, b})
	assert.True(t, strings.HasPrefix(got, "[slack-agent-chat] 2 pending messages, oldest first:\n\n"))
	assert.Contains(t, got, a.Format()+"\n\n---\n\n"+b.Format())
}

func TestRenderMentions(t *testing.T) {
	names := map[string]string{"UCR": "codex-r"}
	got := RenderMentions("<@UCR> and <@UX|x>", func(id string) string { return names[id] })
	assert.Equal(t, "@codex-r and <@UX|x>", got)
}
