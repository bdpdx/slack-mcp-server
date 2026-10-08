package agentchat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// denied-hook (Claude Code PermissionDenied, auto mode only) asks the owner in
// the session's direct channel whether the agent may retry an action the
// auto-mode classifier blocked. A classifier block shows no permission
// prompt, so approval-hook never sees it. The host waits for this hook: on
// Approve it answers retry, and Claude Code tells the model it may retry the
// call (the classifier judges the retry again; the denial itself is not
// reversed). Decline, no answer within the wait, or a host that stops the
// hook leaves the denial standing.
//
// Approval works as for approval-hook: only a click can approve, replies can
// decline, and the listener redraws a request whose hook went away.

// noVerdictPrefixes open the reasons of denials without a classifier verdict,
// for which Claude Code ignores retry: nothing to ask about.
var noVerdictPrefixes = []string{"Auto mode could not evaluate", "Classifier unavailable"}

// FormatDenied renders a blocked action for the owner, as FormatApproval
// does for a prompt, with the classifier's reason.
func FormatDenied(ownerID, agent, tool string, input []byte, reason string) (msg, unsafe string) {
	msg, unsafe = FormatApproval(ownerID, agent, tool, input)
	head := fmt.Sprintf("<@%s> %s needs your approval: %s", ownerID, slackEscaper.Replace(agent), slackEscaper.Replace(tool))
	body := strings.TrimPrefix(msg, head)
	msg = fmt.Sprintf("<@%s> Auto mode blocked %s: %s", ownerID, slackEscaper.Replace(agent), slackEscaper.Replace(tool)) + body
	if r := strings.TrimSpace(reason); r != "" {
		r, hidden := revealHidden(r)
		if hidden && unsafe == "" {
			unsafe = "its reason contains invisible or text-reordering characters (shown as ⟨U+…⟩)"
		}
		msg += "\nReason: " + slackEscaper.Replace(r)
	}
	return msg, unsafe
}

// deniedBlocks lays out the request: Approve (unless unsafe) and Decline.
func deniedBlocks(text, unsafe, id string, wait time.Duration) []slack.Block {
	var buttons []slack.BlockElement
	note := "Approve lets the agent retry; the classifier judges the retry again. Or reply here: _no_ plus a reason. Only the button can approve."
	if unsafe == "" {
		buttons = append(buttons, approvalButton(id, decisionAllow, "Approve retry", "primary"))
	} else {
		note = fmt.Sprintf("This can't be approved from Slack because %s.", slackEscaper.Replace(unsafe))
	}
	buttons = append(buttons, approvalButton(id, decisionDeny, "Decline", "danger"))
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		slack.NewActionBlock("", buttons...),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType,
			fmt.Sprintf("%s The agent waits up to %s; after that the block stands.", note, wait), false, false)),
	}
}

// deniedOutcome is the line that replaces the buttons once settled.
func deniedOutcome(decision, reason string, wait time.Duration) string {
	switch decision {
	case decisionAllow:
		return "✅ Approved; the agent may retry."
	case decisionDeny, decisionTerminal:
		if reason != "" {
			return "❌ Declined: " + slackEscaper.Replace(reason)
		}
		return "❌ Declined; the block stands."
	}
	return fmt.Sprintf("⏱ No answer after %s; the block stands.", wait)
}

// retryDecision is the hook output letting the model retry.
func retryDecision() map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionDenied", "retry": true}}
}

// deniedHook asks the owner in Slack whether a blocked action may be retried.
// It prints nothing (the block stands) when the session watches no project,
// the denial had no classifier verdict, anything fails, the owner declines,
// the wait runs out or the host stops the hook.
func (c *cli) deniedHook(ctx context.Context, ev hookEvent, wait time.Duration) int {
	for _, p := range noVerdictPrefixes {
		if strings.HasPrefix(strings.TrimSpace(ev.Reason), p) {
			return 0
		}
	}
	setup, cancel := context.WithTimeout(ctx, relayTimeout)
	defer cancel()
	channel, name, me, err := c.directChannel(setup, c.hookSession(ev))
	if err != nil {
		return 0
	}
	text, unsafe := FormatDenied(me.ownerID, me.agentName, ev.ToolName, ev.ToolInput, ev.Reason)
	id := newApprovalID()
	_, ts, err := c.bot.PostMessageContext(setup, channel,
		slack.MsgOptionText(fmt.Sprintf("Auto mode blocked %s: %s", me.agentName, ev.ToolName), false),
		slack.MsgOptionBlocks(deniedBlocks(text, unsafe, id, wait)...))
	if err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: posting blocked-action request to #%s: %v\n", name, err)
		return 0
	}
	if _, err := SendControl(setup, c.home.ControlSocket, ControlRequest{Op: "approval-watch", Approval: id, Channel: channel, TS: ts, Text: text}); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: registering blocked-action request with the listener: %v\n", err)
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, wait)
	decision, reason := c.waitForApproval(waitCtx, channel, ts, id)
	cancelWait()
	if decision == decisionAllow && unsafe != "" {
		decision = "" // there was no Approve button; never approve what was not shown
	}

	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), relayTimeout)
	defer cancelFinish()
	outcome := deniedOutcome(decision, reason, wait)
	if decision == "" && ctx.Err() != nil {
		outcome = endedOutcome
	}
	if _, _, _, err := c.bot.UpdateMessageContext(finish, channel, ts,
		slack.MsgOptionText(outcome, false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
			slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, outcome, false, false)),
		)); err != nil {
		fmt.Fprintf(c.stderr, "slack-agent-chat: updating blocked-action request: %v\n", err)
	}
	if decision == decisionAllow {
		c.printJSON(retryDecision())
	}
	return 0
}
