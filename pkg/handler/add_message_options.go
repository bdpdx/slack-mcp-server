package handler

import (
	"errors"

	"github.com/slack-go/slack"
	slackGoUtil "github.com/takara2314/slack-go-util"
	"go.uber.org/zap"
)

// contentOptions builds the text/blocks options for a posted message. The
// message's text field always carries the source text (for markdown, next to
// the rendered blocks): it is the notification fallback, and event consumers
// such as slack-agent-chat read @mentions from it.
func contentOptions(text, contentType string, blocks []slack.Block, logger *zap.Logger) ([]slack.MsgOption, error) {
	if blocks != nil {
		// Raw blocks provided: use them directly. If text is also provided, it
		// serves as the notification/fallback text.
		options := []slack.MsgOption{slack.MsgOptionBlocks(blocks...)}
		if text != "" {
			options = append(options, slack.MsgOptionText(text, false))
		}
		return options, nil
	}
	switch contentType {
	case "text/plain":
		return []slack.MsgOption{slack.MsgOptionDisableMarkdown(), slack.MsgOptionText(text, false)}, nil
	case "text/markdown":
		converted, err := slackGoUtil.ConvertMarkdownTextToBlocks(text)
		if err != nil {
			logger.Warn("Markdown parsing error", zap.Error(err))
			return []slack.MsgOption{slack.MsgOptionDisableMarkdown(), slack.MsgOptionText(text, false)}, nil
		}
		return []slack.MsgOption{slack.MsgOptionBlocks(converted...), slack.MsgOptionText(text, false)}, nil
	}
	return nil, errors.New("content_type must be either 'text/plain' or 'text/markdown'")
}
