package agentchat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// approval-hook (PermissionRequest) asks the owner in the session's direct
// channel, with Allow / Deny / Answer-in-terminal buttons, and answers the
// host's permission prompt on their behalf. The host shows its own prompt
// only after the hook returns, so the hook waits a limited time and then
// hands the prompt back to the terminal.
//
// Button clicks reach this home's listener (the app's Socket Mode
// connection), which checks they came from the owner; the hook polls it.
// The owner can also reply in the message's thread: "yes"/"no ..." etc.

const (
	approvalActionPrefix = "sac-approval-"
	decisionAllow        = "allow"
	decisionDeny         = "deny"
	decisionTerminal     = "terminal"
	defaultApprovalWait  = 10 * time.Minute
	approvalPoll         = 1500 * time.Millisecond
)

var (
	allowWords   = map[string]bool{"yes": true, "y": true, "yep": true, "ok": true, "okay": true, "allow": true, "approve": true, "approved": true, "lgtm": true, "go": true, "sure": true}
	denyWords    = map[string]bool{"no": true, "n": true, "nope": true, "deny": true, "denied": true, "reject": true, "stop": true}
	leadMentions = regexp.MustCompile(`^(?:\s*<@[^>]+>)+`)
)

// ParseApprovalReply reads an owner's thread reply to an approval request:
// it opens with an allow word, a deny word (anything after it is the reason
// for the agent), or "terminal". Any other reply denies, passing the whole
// reply to the agent as the reason.
func ParseApprovalReply(text string) (decision, reason string) {
	text = strings.TrimSpace(leadMentions.ReplaceAllString(text, ""))
	first, rest, _ := strings.Cut(text, " ")
	word := strings.ToLower(strings.Trim(first, ".,!:;-—"))
	rest = strings.TrimSpace(strings.TrimLeft(rest, ".,!:;-— "))
	switch {
	case allowWords[word]:
		return decisionAllow, ""
	case denyWords[word]:
		return decisionDeny, rest
	case word == "terminal":
		return decisionTerminal, ""
	}
	return decisionDeny, text
}

// ownerReplyDecision returns the decision in the first reply from ownerID
// among a thread's messages (the parent first, as Slack returns them).
func ownerReplyDecision(msgs []slack.Message, ownerID string) (decision, reason string, ok bool) {
	for i, m := range msgs {
		if i == 0 || m.User != ownerID || strings.TrimSpace(m.Text) == "" {
			continue
		}
		decision, reason = ParseApprovalReply(m.Text)
		return decision, reason, true
	}
	return "", "", false
}

func approvalButton(id, decision, label, style string) *slack.ButtonBlockElement {
	b := slack.NewButtonBlockElement(approvalActionPrefix+decision, id, slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
	if style != "" {
		b.Style = slack.Style(style)
	}
	return b
}

func approvalBlocks(text, id string, wait time.Duration) []slack.Block {
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		slack.NewActionBlock("",
			approvalButton(id, decisionAllow, "Allow", "primary"),
			approvalButton(id, decisionDeny, "Deny", "danger"),
			approvalButton(id, decisionTerminal, "Answer in terminal", ""),
		),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf(
			"Or reply in the thread: _yes_, or _no_ plus a reason for the agent. After %s with no answer, the terminal asks.", wait), false, false)),
	}
}

// approvalOutcome is the line that replaces the buttons once settled.
func approvalOutcome(decision, reason string, wait time.Duration) string {
	switch decision {
	case decisionAllow:
		return "✅ Allowed."
	case decisionDeny:
		if reason != "" {
			return "❌ Denied: " + slackEscaper.Replace(reason)
		}
		return "❌ Denied."
	case decisionTerminal:
		return "↩️ Answer it in the terminal."
	}
	return fmt.Sprintf("⏱ No answer after %s; answer it in the terminal.", wait)
}

// permissionDecision is the hook output answering the host's prompt.
func permissionDecision(decision, reason string) map[string]any {
	d := map[string]any{"behavior": decision}
	if decision == decisionDeny {
		msg := "The user denied this in Slack."
		if reason != "" {
			msg = "The user denied this in Slack: " + reason
		}
		d["message"] = msg
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": d}}
}

func newApprovalID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// approvalHook asks the owner in Slack and answers the permission prompt.
// It prints nothing, leaving the prompt to the terminal, when the session
// watches no project, anything fails, the owner picks the terminal, or the
// wait runs out.
func (c *cli) approvalHook(ctx context.Context, ev hookEvent, wait time.Duration) int {
	setup, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	channel, name, me, err := c.directChannel(setup, c.hookSession(ev))
	if err != nil {
		return 0
	}
	text := FormatApproval(me.ownerID, me.agentName, ev.ToolName, ev.ToolInput)
	id := newApprovalID()
	_, ts, err := c.bot.PostMessageContext(setup, channel,
		slack.MsgOptionText(fmt.Sprintf("%s needs your approval: %s", me.agentName, ev.ToolName), false),
		slack.MsgOptionBlocks(approvalBlocks(text, id, wait)...))
	if err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: posting approval request to #%s: %v\n", name, err)
		return 0
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, wait)
	decision, reason := c.waitForApproval(waitCtx, channel, ts, id, me.ownerID)
	cancelWait()

	finish, cancelFinish := context.WithTimeout(ctx, relayTimeout)
	defer cancelFinish()
	outcome := approvalOutcome(decision, reason, wait)
	if _, _, _, err := c.bot.UpdateMessageContext(finish, channel, ts,
		slack.MsgOptionText(outcome, false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
			slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, outcome, false, false)),
		)); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: updating approval request: %v\n", err)
	}
	if decision == decisionAllow || decision == decisionDeny {
		c.printJSON(permissionDecision(decision, reason))
	}
	return 0
}

// waitForApproval polls for a button click (via the listener) or an owner
// reply in the thread until one arrives or ctx ends ("" decision).
func (c *cli) waitForApproval(ctx context.Context, channel, ts, id, ownerID string) (string, string) {
	tick := time.NewTicker(approvalPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ""
		case <-tick.C:
		}
		if resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "approval", Approval: id}); err == nil && resp.Decision != "" {
			return resp.Decision, ""
		}
		msgs, _, _, err := c.bot.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channel, Timestamp: ts})
		if err == nil {
			if decision, reason, ok := ownerReplyDecision(msgs, ownerID); ok {
				return decision, reason
			}
		}
	}
}
