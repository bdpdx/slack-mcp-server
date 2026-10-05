package agentchat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Deliverer pushes notice text into one session.
type Deliverer interface {
	Deliver(ctx context.Context, sub *Subscription, clientMsgID, text string) error
}

// HostDeliverer delivers to real Codex and Claude Code sessions.
type HostDeliverer struct {
	CodexSocket string
	CodexHome   string
}

// Deliver routes by session kind; Codex falls back to `codex queue` when the
// daemon is unreachable or the thread is not loaded on it.
func (d *HostDeliverer) Deliver(ctx context.Context, sub *Subscription, clientMsgID, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch sub.Kind {
	case KindClaude:
		return DeliverClaude(ctx, sub.Socket, sub.Token, text)
	case KindCodex:
		err := DeliverCodex(ctx, d.CodexSocket, sub.ThreadID, clientMsgID, text)
		if errors.Is(err, ErrThreadNotLoaded) || errors.Is(err, ErrCodexUnavailable) {
			return QueueCodex(ctx, d.CodexHome, sub.ThreadID, text)
		}
		return err
	}
	return fmt.Errorf("unknown subscription kind %q", sub.Kind)
}

// clientMessageID is a stable ID for pushing message ts in channel to session.
func clientMessageID(session, channel, ts string) string {
	return "slack-agent-chat-" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(session+"\x00"+channel+"\x00"+ts)).String()
}
