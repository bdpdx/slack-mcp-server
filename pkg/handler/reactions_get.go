package handler

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/gocarina/gocsv"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// ReactionRow is one emoji reaction by one user, as reactions_get lists it.
type ReactionRow struct {
	Emoji    string `json:"emoji" csv:"Emoji"`
	UserID   string `json:"user_id" csv:"UserID"`
	UserName string `json:"user_name" csv:"UserName"`
	RealName string `json:"real_name" csv:"RealName"`
}

type reactionsGetParams struct {
	channel   string
	timestamp string
}

// ReactionsGetHandler lists who reacted to a message, and with what
// (reactions.get, which needs the reactions:read scope).
func (ch *ConversationsHandler) ReactionsGetHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ReactionsGetHandler called", zap.Any("params", request.Params))
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}
	params, err := ch.parseParamsToolReactionsGet(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse reactions_get params", zap.Error(err))
		return nil, err
	}
	item, err := ch.apiProvider.Slack().GetReactionsContext(ctx, slack.ItemRef{Channel: params.channel, Timestamp: params.timestamp}, slack.GetReactionsParameters{Full: true})
	if err != nil {
		ch.logger.Error("Slack GetReactionsContext failed", zap.Error(err))
		return nil, err
	}
	resolver := ch.newUserResolver(ctx)
	rows := reactionRows(item.Reactions, resolver.resolve)
	if len(rows) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("No reactions on message %s in channel %s", params.timestamp, params.channel)), nil
	}
	csvBytes, err := gocsv.MarshalBytes(&rows)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}

func (ch *ConversationsHandler) parseParamsToolReactionsGet(ctx context.Context, request mcp.CallToolRequest) (*reactionsGetParams, error) {
	channel := request.GetString("channel_id", "")
	if channel == "" {
		return nil, errors.New("channel_id is required")
	}
	channel, err := ch.resolveChannelID(ctx, channel)
	if err != nil {
		return nil, err
	}
	timestamp := request.GetString("timestamp", "")
	if timestamp == "" {
		return nil, errors.New("timestamp is required")
	}
	if _, err := strconv.ParseFloat(timestamp, 64); err != nil {
		return nil, fmt.Errorf("invalid timestamp %q: use the message ts, like 1234567890.123456", timestamp)
	}
	return &reactionsGetParams{channel: channel, timestamp: timestamp}, nil
}

// reactionRows lists each emoji's reacting users, one row each, named by
// resolve; an unknown user is named by its ID.
func reactionRows(reactions []slack.ItemReaction, resolve func(string) (string, string, bool)) []ReactionRow {
	var rows []ReactionRow
	for _, r := range reactions {
		for _, id := range r.Users {
			name, real, ok := resolve(id)
			if !ok || name == "" {
				name, real = id, id
			}
			rows = append(rows, ReactionRow{Emoji: r.Name, UserID: id, UserName: name, RealName: real})
		}
	}
	return rows
}
