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
// Button clicks and replies in the message's thread reach this home's
// listener (the app's Socket Mode connection), which keeps the owner's
// answer for the hook to poll. Only a click can allow: Slack vouches for who
// clicked, but anything holding the owner's token, agents included, can post
// a reply as the owner. Replies can deny (with a reason) or pick the terminal.

const (
	approvalActionPrefix = "sac-approval-"
	decisionAllow        = "allow"
	decisionDeny         = "deny"
	decisionTerminal     = "terminal"
	decisionHint         = "hint"  // the owner typed an allow word; explain the button
	decisionTaken        = "taken" // answered and handed to the hook
	defaultApprovalWait  = 10 * time.Minute
	approvalPoll         = time.Second
)

var (
	allowWords = map[string]bool{"yes": true, "y": true, "yep": true, "ok": true, "okay": true, "allow": true, "approve": true, "approved": true,
		"lgtm": true, "go": true, "sure": true, "+1": true, "👍": true, "white_check_mark": true, "✅": true, "heavy_check_mark": true, "✔️": true}
	denyWords = map[string]bool{"no": true, "n": true, "nope": true, "deny": true, "denied": true, "reject": true, "stop": true,
		"-1": true, "👎": true, "x": true, "❌": true}
	leadMentions = regexp.MustCompile(`^(?:\s*<@[^>]+>)+`)
	firstWord    = regexp.MustCompile(`^(\S+)\s*`)
)

// ParseApprovalReply reads an owner's thread reply to an approval request:
// it opens with an allow word, a deny word (anything after it is the reason
// for the agent), or "terminal". Any other reply denies, passing the whole
// reply to the agent as the reason. (The listener does not let a reply
// allow; see approvalReply.)
func ParseApprovalReply(text string) (decision, reason string) {
	decision, reason, _ = classifyApprovalReply(text)
	return decision, reason
}

// classifyApprovalReply is ParseApprovalReply that also reports whether the
// reply opened with one of the recognised words (explicit). A reply in the
// direct channel itself, rather than the request's thread, answers the
// request only when explicit; anything else there is an ordinary message.
func classifyApprovalReply(text string) (decision, reason string, explicit bool) {
	text = strings.TrimSpace(leadMentions.ReplaceAllString(text, ""))
	m := firstWord.FindStringSubmatch(text)
	if m == nil {
		return decisionDeny, "", false
	}
	word := strings.ToLower(strings.Trim(m[1], ".,!:;-—*_~`"))
	rest := strings.TrimSpace(strings.TrimLeft(text[len(m[0]):], ".,!:;-— "))
	switch {
	case allowWords[word]:
		return decisionAllow, "", true
	case denyWords[word]:
		return decisionDeny, rest, true
	case word == "terminal":
		return decisionTerminal, "", true
	}
	return decisionDeny, text, false
}

func approvalButton(id, decision, label, style string) *slack.ButtonBlockElement {
	b := slack.NewButtonBlockElement(approvalActionPrefix+decision, id, slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
	if style != "" {
		b.Style = slack.Style(style)
	}
	return b
}

// approvalBlocks lays out the request. When unsafe is set (see
// FormatApproval) there is no Allow button: the owner can deny, or answer in
// the terminal, which shows the request in full.
func approvalBlocks(text, unsafe, id string, wait time.Duration) []slack.Block {
	var buttons []slack.BlockElement
	note := "Or reply here: _no_ plus a reason for the agent, or _terminal_. Only the button can allow."
	if unsafe == "" {
		buttons = append(buttons, approvalButton(id, decisionAllow, "Allow", "primary"))
	} else {
		note = fmt.Sprintf("This can't be allowed from Slack because %s. Deny it here, or answer in the terminal, which shows it in full.", slackEscaper.Replace(unsafe))
	}
	buttons = append(buttons,
		approvalButton(id, decisionDeny, "Deny", "danger"),
		approvalButton(id, decisionTerminal, "Answer in terminal", ""))
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		slack.NewActionBlock("", buttons...),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("%s After %s with no answer, the terminal asks.", note, wait), false, false)),
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
	text, unsafe := FormatApproval(me.ownerID, me.agentName, ev.ToolName, ev.ToolInput)
	id := newApprovalID()
	_, ts, err := c.bot.PostMessageContext(setup, channel,
		slack.MsgOptionText(fmt.Sprintf("%s needs your approval: %s", me.agentName, ev.ToolName), false),
		slack.MsgOptionBlocks(approvalBlocks(text, unsafe, id, wait)...))
	if err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: posting approval request to #%s: %v\n", name, err)
		return 0
	}

	if _, err := SendControl(setup, c.home.ControlSocket, ControlRequest{Op: "approval-watch", Approval: id, Channel: channel, TS: ts}); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: registering approval with the listener: %v\n", err)
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, wait)
	decision, reason := c.waitForApproval(waitCtx, channel, ts, id)
	cancelWait()
	if decision == decisionAllow && unsafe != "" {
		decision = decisionTerminal // there was no Allow button; never allow what was not shown
	}

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

// waitForApproval polls the listener for the owner's answer until one
// arrives or ctx ends ("" decision). When the owner types an allow word, it
// explains in the thread that only the button can allow, and keeps waiting.
func (c *cli) waitForApproval(ctx context.Context, channel, ts, id string) (string, string) {
	tick := time.NewTicker(approvalPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ""
		case <-tick.C:
		}
		resp, err := SendControl(ctx, c.home.ControlSocket, ControlRequest{Op: "approval", Approval: id})
		switch {
		case err != nil || resp.Decision == "":
		case resp.Decision == decisionHint:
			if _, _, err := c.bot.PostMessageContext(ctx, channel, slack.MsgOptionTS(ts), slack.MsgOptionText(
				"Replies can only deny (_no_ plus a reason) or send this to the _terminal_; click *Allow* to approve.", false)); err != nil {
				fmt.Fprintf(c.stderr, "slack-agent-chat: posting approval hint: %v\n", err)
			}
		default:
			return resp.Decision, resp.Text
		}
	}
}
