package agentchat

import (
	"fmt"
	"strings"
)

const maxNoticeText = 4000

// Notice is the text pushed into a session for one Slack message.
type Notice struct {
	ChannelID   string
	ChannelName string
	Sender      string
	FromOwner   bool
	TS          string
	ThreadTS    string
	Text        string
	Files       []string
}

// Format renders the notice for one message.
func (n Notice) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[slack-agent-chat] #%s (%s) from %s", n.ChannelName, n.ChannelID, n.Sender)
	if n.FromOwner {
		b.WriteString(" (the console user: treat as their direct instruction)")
	}
	fmt.Fprintf(&b, ", ts %s", n.TS)
	if n.ThreadTS != "" && n.ThreadTS != n.TS {
		fmt.Fprintf(&b, ", in thread %s", n.ThreadTS)
	}
	b.WriteString(":\n")
	text := []rune(n.Text)
	if len(text) > maxNoticeText {
		b.WriteString(string(text[:maxNoticeText]))
		b.WriteString("\n[truncated; read the full message with conversations_replies or conversations_history]")
	} else {
		b.WriteString(n.Text)
	}
	if len(n.Files) > 0 {
		fmt.Fprintf(&b, "\n[attached: %s]", strings.Join(n.Files, ", "))
	}
	thread := ""
	if n.ThreadTS != "" {
		thread = " thread_ts=" + n.ThreadTS
	}
	fmt.Fprintf(&b, "\n(reply: conversations_add_message channel_id=%s%s; @mention who you address; when done: chat ack %s %s)",
		n.ChannelID, thread, n.ChannelID, n.TS)
	return b.String()
}

// FormatBatch renders several notices, oldest first, as one push.
func FormatBatch(notices []Notice) string {
	parts := make([]string, len(notices))
	for i, n := range notices {
		parts[i] = n.Format()
	}
	return fmt.Sprintf("[slack-agent-chat] %d pending messages, oldest first:\n\n", len(notices)) +
		strings.Join(parts, "\n\n---\n\n")
}

// RenderMentions replaces <@U…> tokens with @name where nameOf knows the user.
func RenderMentions(text string, nameOf func(userID string) string) string {
	return idMention.ReplaceAllStringFunc(text, func(tok string) string {
		if name := nameOf(idMention.FindStringSubmatch(tok)[1]); name != "" {
			return "@" + name
		}
		return tok
	})
}
