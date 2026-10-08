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

// ownerMarker follows the notice prefix on messages from the owner. It comes
// before every field another person controls, so a display name cannot fake it.
const ownerMarker = "[console user]"

// headerSafe strips what a display name or file name would need to imitate
// header syntax: brackets, parentheses and line breaks.
var headerSafe = strings.NewReplacer("[", "", "]", "", "(", "", ")", "", "\n", " ", "\r", " ")

// Format renders the notice for one message. How to reply and acknowledge is
// in the slack-agent-chat skill, not repeated in every notice.
func (n Notice) Format() string {
	var b strings.Builder
	b.WriteString(noticeMarker + " ")
	if n.FromOwner {
		b.WriteString(ownerMarker + " ")
	}
	fmt.Fprintf(&b, "#%s (%s) from %s, ts %s", n.ChannelName, n.ChannelID, strings.TrimSpace(headerSafe.Replace(n.Sender)), n.TS)
	if n.ThreadTS != "" {
		fmt.Fprintf(&b, ", in thread %s", n.ThreadTS)
	}
	b.WriteString(":\n")
	text, truncated := []rune(n.Text), false
	if len(text) > maxNoticeText {
		text, truncated = text[:maxNoticeText], true
	}
	// Quote every body line so message text can never pass for a notice header.
	b.WriteString("> " + strings.ReplaceAll(string(text), "\n", "\n> "))
	if truncated {
		b.WriteString("\n[truncated; read the full message with conversations_replies or conversations_history]")
	}
	if len(n.Files) > 0 {
		names := make([]string, len(n.Files))
		for i, f := range n.Files {
			names[i] = headerSafe.Replace(f)
		}
		fmt.Fprintf(&b, "\n> [attached: %s]", strings.Join(names, ", "))
	}
	return b.String()
}

// FormatBatch renders several notices, oldest first, as one push.
func FormatBatch(notices []Notice) string {
	return FormatBatchPart(notices, 1, 1)
}

// FormatBatchPart renders one part of a backlog delivered in parts (oldest
// first, part of parts). The header tells the agent to read everything
// before acting, since a later message may change or cancel an earlier one.
func FormatBatchPart(notices []Notice, part, parts int) string {
	texts := make([]string, len(notices))
	for i, n := range notices {
		texts[i] = n.Format()
	}
	head := fmt.Sprintf("[slack-agent-chat] %d pending messages, oldest first. Read them all before acting on any: a later message may change or cancel an earlier one.", len(notices))
	if parts > 1 {
		head = fmt.Sprintf("[slack-agent-chat] Pending messages, part %d of %d (%d here), oldest first. ", part, parts, len(notices))
		if part < parts {
			head += "More follow in the next notices: read every part before acting on any message, since a later one may change or cancel an earlier one."
		} else {
			head += "This is the last part: now act on the messages from all parts, with later ones taking precedence over earlier ones."
		}
	}
	return head + "\n\n" + strings.Join(texts, "\n\n---\n\n")
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
