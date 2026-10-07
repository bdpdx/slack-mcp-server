package agentchat

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Deliverer pushes notice text into one session and checks whether the
// session still exists.
type Deliverer interface {
	// Deliver returns the delivery method used; ErrSessionGone means the
	// session has ended.
	Deliver(ctx context.Context, sub *Subscription, clientMsgID, text string) (string, error)
	// Alive reports whether the session still exists; an error means unknown.
	Alive(ctx context.Context, sub *Subscription) (bool, error)
}

// HostDeliverer delivers to real Codex and Claude Code sessions.
type HostDeliverer struct {
	CodexSocket string
}

// Deliver routes by session kind.
func (d *HostDeliverer) Deliver(ctx context.Context, sub *Subscription, clientMsgID, text string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch sub.Kind {
	case KindClaude:
		return "inbox", DeliverClaude(ctx, sub.Socket, sub.Token, text)
	case KindCodex:
		return DeliverCodex(ctx, d.CodexSocket, sub.ThreadID, clientMsgID, text)
	}
	return "", fmt.Errorf("unknown subscription kind %q", sub.Kind)
}

// Alive checks the session without delivering anything.
func (d *HostDeliverer) Alive(ctx context.Context, sub *Subscription) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch sub.Kind {
	case KindClaude:
		return ClaudeSessionAlive(sub.Socket), nil
	case KindCodex:
		return CodexThreadAlive(ctx, d.CodexSocket, sub.ThreadID)
	}
	return false, fmt.Errorf("unknown subscription kind %q", sub.Kind)
}

// clientMessageID is a stable ID for pushing message ts in channel to session.
func clientMessageID(session, channel, ts string) string {
	return "slack-agent-chat-" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(session+"\x00"+channel+"\x00"+ts)).String()
}

// WaitingOnApproval reports whether sub's session is stuck on an approval
// prompt. Only Codex threads expose that (through thread/read); Claude
// sessions report it themselves through the approval hook.
func (d *HostDeliverer) WaitingOnApproval(ctx context.Context, sub *Subscription) (bool, error) {
	if sub.Kind != KindCodex {
		return false, nil
	}
	waiting, known, err := CodexWaitingOnApproval(ctx, d.CodexSocket, sub.ThreadID)
	if err != nil {
		return false, err
	}
	if !known {
		return false, errApprovalUnobservable
	}
	return waiting, nil
}
