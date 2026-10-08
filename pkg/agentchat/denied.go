package agentchat

import (
	"context"
	"fmt"
	"regexp"
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

// noVerdictPrefixes open the documented reasons of denials without a
// classifier verdict (it could not evaluate the action, or was unavailable),
// for which Claude Code ignores retry: nothing to ask about.
var noVerdictPrefixes = []string{"Auto mode could not evaluate", "Classifier unavailable"}

// classifierRule finds the rule a classifier verdict names in brackets, such
// as "[Data Exfiltration]". A verdict without one (Claude Code also reports
// "Blocked by classifier") is still asked about, but its input is withheld:
// nothing says what the classifier objected to.
var classifierRule = regexp.MustCompile(`\[[^\[\]]+\]`)

// exfiltrationRule marks verdicts whose tool input must not be relayed: the
// classifier judged that it should not leave the machine.
var exfiltrationRule = regexp.MustCompile(`(?i)\[[^\]]*exfiltration[^\]]*\]`)

// FormatDenied renders a blocked action for the owner, as FormatApproval
// does for a prompt, with the classifier's reason. For an exfiltration
// verdict, or one naming no rule, the input is withheld, and the request
// cannot be approved from Slack (never approve what was not shown).
func FormatDenied(ownerID, agent, tool string, input []byte, reason string) (msg, unsafe string) {
	msg = fmt.Sprintf("<@%s> Auto mode blocked %s: %s", ownerID, slackEscaper.Replace(agent), slackEscaper.Replace(tool))
	switch {
	case exfiltrationRule.MatchString(reason):
		msg += "\n_The input is withheld: the classifier judged it should not leave this machine. Review it in the session._"
		unsafe = "its input is withheld (an exfiltration verdict)"
	case !classifierRule.MatchString(reason):
		msg += "\n_The input is withheld: the block names no rule, so it may be sensitive. Review it in the session._"
		unsafe = "its input is withheld (the block names no rule)"
	default:
		var detail string
		detail, unsafe = toolDetail(input)
		msg += detail
	}
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

// deniedOutcome is the line that replaces the buttons once settled. A reply
// of "terminal" declines: there is no terminal prompt to hand it to.
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
	if unsafe != "" {
		// Nothing here could approve it, so the agent does not wait: the
		// owner is told, and the block stands.
		line := fmt.Sprintf("The block stands: it can't be approved from Slack because %s.", slackEscaper.Replace(unsafe))
		if _, _, err := c.bot.PostMessageContext(setup, channel,
			slack.MsgOptionText(fmt.Sprintf("Auto mode blocked %s: %s", me.agentName, ev.ToolName), false),
			slack.MsgOptionBlocks(
				slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
				slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, line, false, false)),
			)); err != nil {
			fmt.Fprintf(c.stderr, "slack-agent-chat: posting blocked-action notice to #%s: %v\n", name, err)
		}
		return 0
	}
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
	decision, reason := c.waitForApproval(waitCtx, channel, ts, id, text, deniedHint)
	cancelWait()

	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), relayTimeout)
	defer cancelFinish()
	outcome := deniedOutcome(decision, reason, wait)
	if decision == decisionEnded || (decision == "" && ctx.Err() != nil) {
		outcome = endedOutcome
	}
	c.finishRequest(finish, channel, ts, id, text, outcome)
	if decision == decisionAllow {
		c.printJSON(retryDecision())
	}
	return 0
}
