package agentchat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

// seenResult is `chat seen`'s answer: the agents a message was delivered
// to (their listener reacted 👀 once a live session had it) and the ones
// that processed it (✅).
type seenResult struct {
	Channel   string   `json:"channel"`
	TS        string   `json:"ts"`
	Delivered []string `json:"delivered"`
	Acked     []string `json:"acked"`
}

// reactionsGetter is reactions.get, which needs the reactions:read scope.
type reactionsGetter interface {
	GetReactionsContext(ctx context.Context, item slack.ItemRef, params slack.GetReactionsParameters) (slack.ReactedItem, error)
}

// seenOn names who reacted 👀 and ✅ to message ts in channel. It asks
// reactions.get, which finds any message, thread replies included; an app
// without reactions:read falls back to reading the message from history,
// where a reply needs its parent (thread).
func seenOn(ctx context.Context, api SlackAPI, channel, ts, thread string) (seenResult, error) {
	if rg, ok := api.(reactionsGetter); ok {
		item, err := rg.GetReactionsContext(ctx, slack.ItemRef{Channel: channel, Timestamp: ts}, slack.GetReactionsParameters{Full: true})
		switch {
		case err == nil:
			return seenFrom(ctx, api, channel, ts, item.Reactions), nil
		case !strings.Contains(err.Error(), "missing_scope"):
			return seenResult{}, err
		}
	}
	var found *slack.Message
	if thread == "" {
		resp, err := api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: channel, Latest: ts, Inclusive: true, Limit: 1})
		if err != nil {
			return seenResult{}, err
		}
		for i := range resp.Messages {
			if resp.Messages[i].Timestamp == ts {
				found = &resp.Messages[i]
			}
		}
		if found == nil {
			return seenResult{}, fmt.Errorf("no message %s in %s; for a thread reply, pass --thread PARENT_TS", ts, channel)
		}
	} else {
		replies, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channel, Timestamp: thread, Latest: ts, Inclusive: true})
		if err != nil {
			return seenResult{}, err
		}
		for i := range replies {
			if replies[i].Timestamp == ts {
				found = &replies[i]
			}
		}
		if found == nil {
			return seenResult{}, fmt.Errorf("no reply %s in thread %s of %s", ts, thread, channel)
		}
	}
	return seenFrom(ctx, api, channel, ts, found.Reactions), nil
}

// seenFrom names the users in reactions' 👀 and ✅.
func seenFrom(ctx context.Context, api SlackAPI, channel, ts string, reactions []slack.ItemReaction) seenResult {
	res := seenResult{Channel: channel, TS: ts, Delivered: []string{}, Acked: []string{}}
	for _, r := range reactions {
		var to *[]string
		switch r.Name {
		case reactionDelivered:
			to = &res.Delivered
		case reactionAcked:
			to = &res.Acked
		default:
			continue
		}
		for _, id := range r.Users {
			name := id
			if u, err := api.GetUserInfoContext(ctx, id); err == nil {
				name = shownName(u) // the agent name whoami and the rosters use
			}
			*to = append(*to, name)
		}
	}
	return res
}

// seen runs `chat seen CHANNEL TS [--thread PARENT_TS]`.
func (c *cli) seen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seen", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	thread := fs.String("thread", "", "the thread's parent ts, when TS is a reply (needed only without reactions:read)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: seen CHANNEL TS [--thread PARENT_TS]")
	}
	if _, err := strconv.ParseFloat(pos[1], 64); err != nil {
		return fmt.Errorf("invalid ts %q", pos[1])
	}
	id, _, err := c.resolveChannel(ctx, pos[0])
	if err != nil {
		return err
	}
	res, err := seenOn(ctx, c.bot, id, pos[1], *thread)
	if err != nil {
		return err
	}
	c.printJSON(res)
	return nil
}
