package agentchat

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chat seen names the agents a message was delivered to (their listener's
// 👀) and the ones that processed it (✅), from the message's reactions.
func TestSeen(t *testing.T) {
	ctx := context.Background()
	api := newFakeSlack()
	online := msg("2000.5", "UCL", "online")
	online.Reactions = []slack.ItemReaction{
		{Name: reactionDelivered, Users: []string{"UCB", "UCR"}},
		{Name: reactionAcked, Users: []string{"UCB"}},
		{Name: "thumbsup", Users: []string{"UBR"}},
	}
	api.history["C1"] = []slack.Message{msg("2000.6", "UBR", "later"), online, msg("2000.4", "UBR", "earlier")}
	got, err := seenOn(ctx, api, "C1", "2000.5", "")
	require.NoError(t, err)
	assert.Equal(t, seenResult{Channel: "C1", TS: "2000.5", Delivered: []string{"codex-b", "codex-r"}, Acked: []string{"codex-b"}}, got)

	reply := msg("2000.8", "UCL", "in thread")
	reply.ThreadTimestamp = "2000.4"
	reply.Reactions = []slack.ItemReaction{{Name: reactionDelivered, Users: []string{"UCR"}}}
	api.replies["C1|2000.4"] = []slack.Message{msg("2000.4", "UBR", "earlier"), reply}
	got, err = seenOn(ctx, api, "C1", "2000.8", "2000.4")
	require.NoError(t, err)
	assert.Equal(t, []string{"codex-r"}, got.Delivered)
	assert.Empty(t, got.Acked)

	_, err = seenOn(ctx, api, "C1", "2000.8", "")
	assert.ErrorContains(t, err, "--thread")
}
