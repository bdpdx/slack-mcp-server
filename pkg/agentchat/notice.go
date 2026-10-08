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
	return fmt.Sprintf("[slack-agent-chat] %d pending messages, oldest first. Read them all before acting on any: a later message may change or cancel an earlier one.\n\n", len(notices)) +
		joinNotices(notices)
}

// FormatCatchUp renders part of parts of a catch-up: messages start..
// of total, oldest first. The header gives the totals so the agent reads
// everything before acting, and the first part says how many older
// unacknowledged messages (skipped) were not sent.
func FormatCatchUp(notices []Notice, part, parts, start, total, skipped int) string {
	end := start + len(notices) - 1
	head := fmt.Sprintf("[slack-agent-chat] Catch-up: %d messages, oldest first.", total)
	if parts > 1 {
		head = fmt.Sprintf("[slack-agent-chat] Catch-up: %d messages, oldest first, in %d parts (part %d of %d: messages %d–%d of %d).", total, parts, part, parts, start, end, total)
	}
	if part < parts {
		head += fmt.Sprintf(" More follow in the next notices. Do not act on any message until you have read all %d: a later one may change or cancel an earlier one. If the remaining parts have not arrived within a few minutes, read the rest from the channel history instead.", total)
	} else if parts > 1 {
		head += fmt.Sprintf(" This is the last part: now act on all %d, with later messages taking precedence over earlier ones.", total)
	} else {
		head += fmt.Sprintf(" Read all %d before acting on any: a later one may change or cancel an earlier one.", total)
	}
	if skipped > 0 && part == 1 {
		head += fmt.Sprintf(" %d older unacknowledged messages were not sent; read the channel history if you need them.", skipped)
	}
	return head + "\n\n" + joinNotices(notices)
}

func joinNotices(notices []Notice) string {
	texts := make([]string, len(notices))
	for i, n := range notices {
		texts[i] = n.Format()
	}
	return strings.Join(texts, "\n\n---\n\n")
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
