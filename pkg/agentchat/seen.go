package agentchat

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"

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

// seenOn reads message ts in channel (a reply in thread when thread is set)
// and names who reacted 👀 and ✅. History carries each reaction's users,
// so no reactions:read scope is needed.
func seenOn(ctx context.Context, api SlackAPI, channel, ts, thread string) (seenResult, error) {
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
	res := seenResult{Channel: channel, TS: ts, Delivered: []string{}, Acked: []string{}}
	for _, r := range found.Reactions {
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
			if u, err := api.GetUserInfoContext(ctx, id); err == nil && u.Name != "" {
				name = u.Name
			}
			*to = append(*to, name)
		}
	}
	return res, nil
}

// seen runs `chat seen CHANNEL TS [--thread PARENT_TS]`.
func (c *cli) seen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seen", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	thread := fs.String("thread", "", "the thread's parent ts, when TS is a reply")
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
