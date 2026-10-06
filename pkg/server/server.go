package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/korotovsky/slack-mcp-server/pkg/handler"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/server/auth"
	"github.com/korotovsky/slack-mcp-server/pkg/text"
	"github.com/korotovsky/slack-mcp-server/pkg/toolconfig"
	"github.com/korotovsky/slack-mcp-server/pkg/version"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"
)

type MCPServer struct {
	server *server.MCPServer
	logger *zap.Logger
}

const (
	ToolConversationsHistory        = "conversations_history"
	ToolConversationsReplies        = "conversations_replies"
	ToolConversationsAddMessage     = "conversations_add_message"
	ToolConversationsDeleteMessage  = "conversations_delete_message"
	ToolConversationsOpen           = "conversations_open"
	ToolFilesUpload                 = "files_upload"
	ToolReactionsAdd                = "reactions_add"
	ToolReactionsRemove             = "reactions_remove"
	ToolAttachmentGetData           = "attachment_get_data"
	ToolConversationsSearchMessages = "conversations_search_messages"
	ToolConversationsUnreads        = "conversations_unreads"
	ToolConversationsMark           = "conversations_mark"
	ToolConversationsLeave          = "conversations_leave"
	ToolConversationsJoin           = "conversations_join"
	ToolConversationsRename         = "conversations_rename"
	ToolConversationsCreate         = "conversations_create"
	ToolConversationsSetTopic       = "conversations_set_topic"
	ToolConversationsInvite         = "conversations_invite"
	ToolConversationsInviteShared   = "conversations_invite_shared"
	ToolChannelsList                = "channels_list"
	ToolChannelsMe                  = "channels_me"
	ToolUsergroupsList              = "usergroups_list"
	ToolUsergroupsMe                = "usergroups_me"
	ToolUsergroupsCreate            = "usergroups_create"
	ToolUsergroupsUpdate            = "usergroups_update"
	ToolUsergroupsUsersUpdate       = "usergroups_users_update"
	ToolUsersSearch                 = "users_search"
)

var ValidToolNames = []string{
	ToolConversationsHistory,
	ToolConversationsReplies,
	ToolConversationsAddMessage,
	ToolConversationsDeleteMessage,
	ToolConversationsOpen,
	ToolFilesUpload,
	ToolReactionsAdd,
	ToolReactionsRemove,
	ToolAttachmentGetData,
	ToolConversationsSearchMessages,
	ToolConversationsUnreads,
	ToolConversationsMark,
	ToolConversationsLeave,
	ToolConversationsJoin,
	ToolConversationsRename,
	ToolConversationsCreate,
	ToolConversationsSetTopic,
	ToolConversationsInvite,
	ToolConversationsInviteShared,
	ToolChannelsList,
	ToolChannelsMe,
	ToolUsergroupsList,
	ToolUsergroupsMe,
	ToolUsergroupsCreate,
	ToolUsergroupsUpdate,
	ToolUsergroupsUsersUpdate,
	ToolUsersSearch,
}

func ValidateEnabledTools(tools []string) error {
	validToolSet := make(map[string]bool, len(ValidToolNames))
	for _, name := range ValidToolNames {
		validToolSet[name] = true
	}

	var invalidTools []string
	for _, tool := range tools {
		if !validToolSet[tool] {
			invalidTools = append(invalidTools, tool)
		}
	}
	if len(invalidTools) > 0 {
		return fmt.Errorf("invalid tool name(s): %s. Valid tools are: %s",
			strings.Join(invalidTools, ", "),
			strings.Join(ValidToolNames, ", "))
	}
	return nil
}

// NewMCPServer registers the tools that cfg enables. cfg is the same parsed
// configuration the handlers check, so registration and enforcement agree.
func NewMCPServer(provider *provider.ApiProvider, logger *zap.Logger, cfg *toolconfig.Config) *MCPServer {
	opts := append([]server.ServerOption{server.WithLogging(), server.WithRecovery()},
		toolMiddlewares(provider.ServerTransport(), logger)...)
	s := server.NewMCPServer("Slack MCP Server", version.Version, opts...)

	conversationsHandler := handler.NewConversationsHandler(provider, logger, cfg)

	if cfg.ToolEnabled(ToolConversationsHistory) {
		s.AddTool(mcp.NewTool(ToolConversationsHistory,
			mcp.WithDescription("Get messages from the channel (or DM) by channel_id, the last row/column in the response is used as 'cursor' parameter for pagination if not empty"),
			mcp.WithTitleAnnotation("Get Conversation History"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("    - `channel_id` (string): ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... aka #general or @username_dm."),
			),
			mcp.WithBoolean("include_activity_messages",
				mcp.Description("If true, the response will include activity messages such as 'channel_join' or 'channel_leave'. Default is boolean false."),
				mcp.DefaultBool(false),
			),
			mcp.WithString("cursor",
				mcp.Description("Cursor for pagination. Use the value of the last row and column in the response as next_cursor field returned from the previous request."),
			),
			mcp.WithString("limit",
				mcp.DefaultString("1d"),
				mcp.Description("Limit of messages to fetch in format of maximum ranges of time (e.g. 1d - 1 day, 1w - 1 week, 30d - 30 days, 90d - 90 days which is a default limit for free tier history) or number of messages (e.g. 50). Must be empty when 'cursor' is provided."),
			),
		), conversationsHandler.ConversationsHistoryHandler)
	}

	if cfg.ToolEnabled(ToolConversationsReplies) {
		s.AddTool(mcp.NewTool(ToolConversationsReplies,
			mcp.WithDescription("Get a thread of messages posted to a conversation by channelID and thread_ts, the last row/column in the response is used as 'cursor' parameter for pagination if not empty"),
			mcp.WithTitleAnnotation("Get Thread Replies"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... aka #general or @username_dm."),
			),
			mcp.WithString("thread_ts",
				mcp.Required(),
				mcp.Description("Unique identifier of either a thread's parent message or a message in the thread. ts must be the timestamp in format 1234567890.123456 of an existing message with 0 or more replies."),
			),
			mcp.WithBoolean("include_activity_messages",
				mcp.Description("If true, the response will include activity messages such as 'channel_join' or 'channel_leave'. Default is boolean false."),
				mcp.DefaultBool(false),
			),
			mcp.WithString("cursor",
				mcp.Description("Cursor for pagination. Use the value of the last row and column in the response as next_cursor field returned from the previous request."),
			),
			mcp.WithString("limit",
				mcp.DefaultString("1d"),
				mcp.Description("Limit of messages to fetch in format of maximum ranges of time (e.g. 1d - 1 day, 30d - 30 days, 90d - 90 days which is a default limit for free tier history) or number of messages (e.g. 50). Must be empty when 'cursor' is provided."),
			),
		), conversationsHandler.ConversationsRepliesHandler)
	}

	if cfg.ToolEnabled(ToolConversationsAddMessage) {
		s.AddTool(mcp.NewTool(ToolConversationsAddMessage,
			mcp.WithDescription("Add a message to a public channel, private channel, or direct message (DM, or IM) conversation by channel_id and thread_ts."),
			mcp.WithTitleAnnotation("Send Message"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... aka #general or @username_dm."),
			),
			mcp.WithString("thread_ts",
				mcp.Description("Unique identifier of either a thread's parent message or a message in the thread_ts must be the timestamp in format 1234567890.123456 of an existing message with 0 or more replies. Optional, if not provided the message will be added to the channel itself, otherwise it will be added to the thread."),
			),
			mcp.WithString("text",
				mcp.Description("Message text in specified content_type format. Example: 'Hello, world!' for text/plain or '# Hello, world!' for text/markdown."),
			),
			mcp.WithString("content_type",
				mcp.DefaultString("text/markdown"),
				mcp.Description("Content type of the message. Default is 'text/markdown'. Allowed values: 'text/markdown', 'text/plain'. Ignored when blocks is provided."),
			),
			mcp.WithString("blocks",
				mcp.Description("Raw Slack Block Kit JSON array for rich message formatting (rich_text lists, code blocks, etc.). When provided, this takes precedence over text/content_type for rendering. The text parameter becomes the notification fallback text."),
			),
			mcp.WithBoolean("as_user",
				mcp.DefaultBool(false),
				mcp.Description("Post as the user instead of the bot. Set to true only when the user asks for the message to come from them (e.g. 'from me', 'as me', 'on my behalf'). Default false posts as the bot."),
			),
		), conversationsHandler.ConversationsAddMessageHandler)
	}

	if cfg.ToolEnabled(ToolConversationsDeleteMessage) {
		s.AddTool(mcp.NewTool(ToolConversationsDeleteMessage,
			mcp.WithDescription("Delete a message from a public channel, private channel, or direct message (DM, or IM) conversation by channel_id and timestamp."),
			mcp.WithTitleAnnotation("Delete Message"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... aka #general or @username_dm."),
			),
			mcp.WithString("timestamp",
				mcp.Required(),
				mcp.Description("The timestamp of the message to delete in format 1234567890.123456."),
			),
		), conversationsHandler.ConversationsDeleteMessageHandler)
	}

	if cfg.ToolEnabled(ToolConversationsOpen) {
		s.AddTool(mcp.NewTool(ToolConversationsOpen,
			mcp.WithDescription("Open or resume a direct message (1 user) or multi-person direct message / group DM (2+ users). Returns the resulting channel_id, which can then be used with conversations_add_message."),
			mcp.WithTitleAnnotation("Open Conversation"),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithString("users",
				mcp.Required(),
				mcp.Description("Comma-separated list of Slack user IDs, @handles, or emails to open a conversation with, e.g. 'U0123456,U0654321' or '@iffat.hasan,@mary.toledano'. One user opens/resumes a 1:1 DM; two or more open/resume a group DM (mpim)."),
			),
			mcp.WithBoolean("return_im",
				mcp.DefaultBool(false),
				mcp.Description("Whether to return the full IM channel definition in the response. Only meaningful for a single-user (1:1 DM) request."),
			),
		), conversationsHandler.ConversationsOpenHandler)
	}

	if cfg.ToolEnabled(ToolReactionsAdd) {
		s.AddTool(mcp.NewTool(ToolReactionsAdd,
			mcp.WithDescription("Add an emoji reaction to a message in a public channel, private channel, or direct message (DM, or IM) conversation."),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... aka #general or @username_dm."),
			),
			mcp.WithString("timestamp",
				mcp.Required(),
				mcp.Description("Timestamp of the message to add reaction to, in format 1234567890.123456."),
			),
			mcp.WithString("emoji",
				mcp.Required(),
				mcp.Description("The name of the emoji to add as a reaction (without colons). Example: 'thumbsup', 'heart', 'rocket'."),
			),
			mcp.WithBoolean("as_user",
				mcp.DefaultBool(false),
				mcp.Description("React as the user instead of the bot. Set to true only when the user asks for the reaction to come from them (e.g. 'from me', 'as me'). Default false reacts as the bot."),
			),
		), conversationsHandler.ReactionsAddHandler)
	}

	if cfg.ToolEnabled(ToolReactionsRemove) {
		s.AddTool(mcp.NewTool(ToolReactionsRemove,
			mcp.WithDescription("Remove an emoji reaction from a message in a public channel, private channel, or direct message (DM, or IM) conversation."),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... aka #general or @username_dm."),
			),
			mcp.WithString("timestamp",
				mcp.Required(),
				mcp.Description("Timestamp of the message to remove reaction from, in format 1234567890.123456."),
			),
			mcp.WithString("emoji",
				mcp.Required(),
				mcp.Description("The name of the emoji to remove as a reaction (without colons). Example: 'thumbsup', 'heart', 'rocket'."),
			),
			mcp.WithBoolean("as_user",
				mcp.DefaultBool(false),
				mcp.Description("Remove the user's reaction instead of the bot's. Set to true when the reaction was added as the user. Default false removes the bot's reaction."),
			),
		), conversationsHandler.ReactionsRemoveHandler)
	}

	if cfg.ToolEnabled(ToolAttachmentGetData) {
		s.AddTool(mcp.NewTool(ToolAttachmentGetData,
			mcp.WithDescription("Download an attachment's content by file ID. Returns file metadata and content (text files as-is, binary files as base64). Maximum file size is 64 MiB."),
			mcp.WithTitleAnnotation("Get Attachment Data"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("file_id",
				mcp.Required(),
				mcp.Description("The ID of the attachment to download, in format Fxxxxxxxxxx. Attachment IDs (with filenames) can be found in the AttachmentIDs field of message metadata when FileCount > 0."),
			),
		), conversationsHandler.FilesGetHandler)
	}

	if cfg.ToolEnabled(ToolFilesUpload) {
		s.AddTool(mcp.NewTool(ToolFilesUpload,
			mcp.WithDescription("Upload a file and share it to a Slack channel or DM. Provide either UTF-8 text in content or base64-encoded bytes in content_base64; files are limited to 64 MiB. This write tool is disabled unless explicitly enabled."),
			mcp.WithTitleAnnotation("Upload File"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("Channel or DM ID, or a resolvable channel name such as #general or @username_dm."),
			),
			mcp.WithString("filename",
				mcp.Required(),
				mcp.Description("Filename to display in Slack, including its extension."),
			),
			mcp.WithString("content",
				mcp.Description("UTF-8 text file contents. Use either this or content_base64, not both."),
			),
			mcp.WithString("content_base64",
				mcp.Description("Base64-encoded file bytes for binary or arbitrary text files. Use either this or content, not both."),
			),
			mcp.WithString("title",
				mcp.Description("Optional display title. Defaults to filename."),
			),
			mcp.WithString("initial_comment",
				mcp.Description("Optional message to post with the file."),
			),
			mcp.WithString("thread_ts",
				mcp.Description("Optional parent message timestamp to share the file in an existing thread."),
			),
			mcp.WithBoolean("as_user",
				mcp.DefaultBool(false),
				mcp.Description("Upload as the user instead of the bot. Set to true only when the user asks for the file to come from them (e.g. 'from me', 'as me'). Default false uploads as the bot."),
			),
		), conversationsHandler.FilesUploadHandler)
	}

	conversationsSearchTool := mcp.NewTool(ToolConversationsSearchMessages,
		mcp.WithDescription("Search messages in a public channel, private channel, or direct message (DM, or IM) conversation using filters. All filters are optional, if not provided then search_query is required."),
		mcp.WithTitleAnnotation("Search Messages"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("search_query",
			mcp.Description("Search query to filter messages. Example: 'marketing report' or full URL of Slack message e.g. 'https://slack.com/archives/C1234567890/p1234567890123456', then the tool will return a single message matching given URL, herewith all other parameters will be ignored."),
		),
		mcp.WithString("filter_in_channel",
			mcp.Description("Filter messages in a specific public/private channel by its ID or name. Example: 'C1234567890', 'G1234567890', or '#general'. If not provided, all channels will be searched."),
		),
		mcp.WithString("filter_in_im_or_mpim",
			mcp.Description("Filter messages in a direct message (DM) or multi-person direct message (MPIM) conversation by its ID or name. Example: 'D1234567890' or '@username_dm'. If not provided, all DMs and MPIMs will be searched."),
		),
		mcp.WithString("filter_users_with",
			mcp.Description("Filter messages with a specific user by their ID or display name in threads and DMs. Example: 'U1234567890' or '@username'. If not provided, all threads and DMs will be searched."),
		),
		mcp.WithString("filter_users_from",
			mcp.Description("Filter messages from a specific user by their ID or display name. Example: 'U1234567890' or '@username'. If not provided, all users will be searched."),
		),
		mcp.WithString("filter_date_before",
			mcp.Description("Filter messages sent before a specific date in format 'YYYY-MM-DD'. Example: '2023-10-01', 'July', 'Yesterday' or 'Today'. If not provided, all dates will be searched."),
		),
		mcp.WithString("filter_date_after",
			mcp.Description("Filter messages sent after a specific date in format 'YYYY-MM-DD'. Example: '2023-10-01', 'July', 'Yesterday' or 'Today'. If not provided, all dates will be searched."),
		),
		mcp.WithString("filter_date_on",
			mcp.Description("Filter messages sent on a specific date in format 'YYYY-MM-DD'. Example: '2023-10-01', 'July', 'Yesterday' or 'Today'. If not provided, all dates will be searched."),
		),
		mcp.WithString("filter_date_during",
			mcp.Description("Filter messages sent during a specific period in format 'YYYY-MM-DD'. Example: 'July', 'Yesterday' or 'Today'. If not provided, all dates will be searched."),
		),
		mcp.WithBoolean("filter_threads_only",
			mcp.Description("If true, the response will include only messages from threads. Default is boolean false."),
		),
		mcp.WithString("cursor",
			mcp.DefaultString(""),
			mcp.Description("Cursor for pagination. Use the value of the last row and column in the response as next_cursor field returned from the previous request."),
		),
		mcp.WithNumber("limit",
			mcp.DefaultNumber(20),
			mcp.Description("The maximum number of items to return. Must be an integer between 1 and 100."),
		),
	)
	// Only register search tool for non-bot tokens (bot tokens cannot use search.messages API)
	if !provider.UserIsBotToken() && cfg.ToolEnabled(ToolConversationsSearchMessages) {
		s.AddTool(conversationsSearchTool, conversationsHandler.ConversationsSearchHandler)
	}

	if cfg.ToolEnabled(ToolUsersSearch) {
		s.AddTool(mcp.NewTool(ToolUsersSearch,
			mcp.WithDescription("Search for users by name, email, display name, or Slack user ID. If a Slack user ID is provided (e.g. U07VCEPP4N5), the user is looked up directly. Returns user details and DM channel ID if available."),
			mcp.WithTitleAnnotation("Search Users"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Search query - matches against real name, display name, username, email, or a Slack user ID (e.g. U07VCEPP4N5)."),
			),
			mcp.WithNumber("limit",
				mcp.DefaultNumber(10),
				mcp.Description("Maximum number of results to return (1-100). Default is 10."),
			),
		), conversationsHandler.UsersSearchHandler)
	}

	// Register unreads tool - gets all unread messages across channels efficiently.
	// Bot tokens (xoxb) don't support unread tracking, so exclude them (same pattern as search tool).
	if !provider.UserIsBotToken() && cfg.ToolEnabled(ToolConversationsUnreads) {
		s.AddTool(mcp.NewTool(ToolConversationsUnreads,
			mcp.WithDescription("Get unread messages across all channels. Requires a user token (xoxp). Scans a subset of channels per type (limited by max_channels) — results may be partial on large workspaces. Results are prioritized: DMs > group DMs > partner channels > internal channels."),
			mcp.WithTitleAnnotation("Get Unread Messages"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithBoolean("include_messages",
				mcp.Description("If true (default), returns the actual unread messages. If false, returns only a summary of channels with unreads."),
				mcp.DefaultBool(true),
			),
			mcp.WithString("channel_types",
				mcp.Description("Filter by channel type: 'all' (default), 'dm' (direct messages), 'group_dm' (group DMs), 'partner' (ext-* channels), 'internal' (other channels)."),
				mcp.DefaultString("all"),
			),
			mcp.WithNumber("max_channels",
				mcp.Description("Maximum number of channels to fetch unreads from. Default is 50."),
				mcp.DefaultNumber(50),
			),
			mcp.WithNumber("max_messages_per_channel",
				mcp.Description("Maximum messages to fetch per channel. Default is 10."),
				mcp.DefaultNumber(10),
			),
			mcp.WithBoolean("mentions_only",
				mcp.Description("If true, only returns channels where you have @mentions. Default is false."),
				mcp.DefaultBool(false),
			),
		), conversationsHandler.ConversationsUnreadsHandler)
	}

	// Register mark tool - marks a channel as read. It acts as the user, so
	// it is only registered when enabled and a user token is configured.
	if !provider.UserIsBotToken() && cfg.ToolEnabled(ToolConversationsMark) {
		s.AddTool(mcp.NewTool(ToolConversationsMark,
			mcp.WithDescription("Mark a channel or DM as read. If no timestamp is provided, marks all messages as read."),
			mcp.WithTitleAnnotation("Mark as Read"),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... (e.g., #general, @username)."),
			),
			mcp.WithString("ts",
				mcp.Description("Timestamp of the message to mark as read up to. If not provided, marks all messages as read."),
			),
		), conversationsHandler.ConversationsMarkHandler)
	}

	if cfg.ToolEnabled(ToolConversationsLeave) {
		s.AddTool(mcp.NewTool(ToolConversationsLeave,
			mcp.WithDescription("Leave a channel, group conversation, or DM. Cannot leave the #general channel."),
			mcp.WithTitleAnnotation("Leave Channel"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... or @... (e.g., #general, @username)."),
			),
		), conversationsHandler.ConversationsLeaveHandler)
	}

	if cfg.ToolEnabled(ToolConversationsJoin) {
		s.AddTool(mcp.NewTool(ToolConversationsJoin,
			mcp.WithDescription("Join a public channel. Use channels_list or channels_me to find channel IDs."),
			mcp.WithTitleAnnotation("Join Channel"),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... (e.g., #general)."),
			),
		), conversationsHandler.ConversationsJoinHandler)
	}
	if cfg.ToolEnabled(ToolConversationsRename) {
		s.AddTool(mcp.NewTool(ToolConversationsRename,
			mcp.WithDescription("Rename a public or private channel. Requires the acting user to be the channel creator or a workspace admin/owner."),
			mcp.WithTitleAnnotation("Rename Channel"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... (e.g., #general)."),
			),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("New name for the channel, without the leading #. Lowercase, no spaces (use hyphens)."),
			),
		), conversationsHandler.ConversationsRenameHandler)
	}

	if cfg.ToolEnabled(ToolConversationsCreate) {
		s.AddTool(mcp.NewTool(ToolConversationsCreate,
			mcp.WithDescription("Create a new public or private channel. Returns the resulting channel_id, which can then be used with conversations_invite_shared or conversations_add_message."),
			mcp.WithTitleAnnotation("Create Channel"),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("Name for the new channel, without the leading #. Lowercase, no spaces (use hyphens)."),
			),
			mcp.WithBoolean("is_private",
				mcp.DefaultBool(false),
				mcp.Description("Whether the channel should be private. Default is false (public channel)."),
			),
		), conversationsHandler.ConversationsCreateHandler)
	}

	if cfg.ToolEnabled(ToolConversationsSetTopic) {
		s.AddTool(mcp.NewTool(ToolConversationsSetTopic,
			mcp.WithDescription("Set the topic and/or purpose (description) of a channel. At least one of topic or purpose must be provided."),
			mcp.WithTitleAnnotation("Set Channel Topic"),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... (e.g., #general)."),
			),
			mcp.WithString("topic",
				mcp.Description("New topic for the channel. Omit to leave the topic unchanged."),
			),
			mcp.WithString("purpose",
				mcp.Description("New purpose (description) for the channel. Omit to leave the purpose unchanged."),
			),
		), conversationsHandler.ConversationsSetTopicHandler)
	}

	if cfg.ToolEnabled(ToolConversationsInvite) {
		s.AddTool(mcp.NewTool(ToolConversationsInvite,
			mcp.WithDescription("Invite one or more existing workspace members to a public or private channel."),
			mcp.WithTitleAnnotation("Invite Users"),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... (e.g., #general)."),
			),
			mcp.WithString("users",
				mcp.Required(),
				mcp.Description("Comma-separated list of Slack user IDs, @handles, or emails to invite, e.g. 'U0123456,U0654321' or '@iffat.hasan'."),
			),
		), conversationsHandler.ConversationsInviteHandler)
	}

	if cfg.ToolEnabled(ToolConversationsInviteShared) {
		s.AddTool(mcp.NewTool(ToolConversationsInviteShared,
			mcp.WithDescription("Invite external people to a channel via Slack Connect, turning it into a shared channel. This sends a real invite (by email, or directly if the person already has a Slack Connect relationship) that is visible to the recipient outside this workspace - it is not a preview or a draft. Requires the acting user/token to have permission to send Slack Connect invites for this workspace."),
			mcp.WithTitleAnnotation("Invite External User (Slack Connect)"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("channel_id",
				mcp.Required(),
				mcp.Description("ID of the channel in format Cxxxxxxxxxx or its name starting with #... (e.g., #general)."),
			),
			mcp.WithString("emails",
				mcp.Description("Comma-separated list of external email addresses to invite via Slack Connect, e.g. 'ben@platter.com'. Provide either emails or user_ids, not both."),
			),
			mcp.WithString("user_ids",
				mcp.Description("Comma-separated list of Slack user IDs to invite via Slack Connect. Provide either emails or user_ids, not both."),
			),
		), conversationsHandler.ConversationsInviteSharedHandler)
	}

	channelsHandler := handler.NewChannelsHandler(provider, logger)
	usergroupsHandler := handler.NewUsergroupsHandler(provider, logger, cfg)

	if cfg.ToolEnabled(ToolChannelsList) {
		s.AddTool(mcp.NewTool(ToolChannelsList,
			mcp.WithDescription("Get list of channels"),
			mcp.WithTitleAnnotation("List Channels"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("channel_types",
				mcp.Required(),
				mcp.Description("Comma-separated channel types. Allowed values: 'mpim', 'im', 'public_channel', 'private_channel'. Example: 'public_channel,private_channel,im'"),
			),
			mcp.WithString("sort",
				mcp.Description("Type of sorting. Allowed values: 'popularity' - sort by number of members/participants in each channel."),
			),
			mcp.WithNumber("limit",
				mcp.DefaultNumber(100),
				mcp.Description("The maximum number of items to return. Must be an integer between 1 and 1000 (maximum 999)."), // context fix for cursor: https://github.com/korotovsky/slack-mcp-server/issues/7
			),
			mcp.WithString("cursor",
				mcp.Description("Cursor for pagination. Use the value of the last row and column in the response as next_cursor field returned from the previous request."),
			),
			mcp.WithString("query",
				mcp.Description("Optional keyword to filter channels. Case-insensitive substring match against the fields specified by query_targets. Example: 'marketing' returns channels like #marketing, #marketing-ops."),
			),
			mcp.WithString("query_targets",
				mcp.DefaultString("name"),
				mcp.Description("Comma-separated list of fields to match the query against. Allowed values: 'name', 'topic', 'purpose'. Example: 'name,topic,purpose' to search all fields. Default is 'name'."),
			),
		), channelsHandler.ChannelsHandler)
	}

	if cfg.ToolEnabled(ToolChannelsMe) {
		s.AddTool(mcp.NewTool(ToolChannelsMe,
			mcp.WithDescription("List channels you are a member of. Unlike channels_list which returns all workspace channels, this returns only channels you have joined. Useful on large workspaces where channels_list returns thousands of results."),
			mcp.WithTitleAnnotation("My Channels"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithString("channel_types",
				mcp.Description("Comma-separated channel types. Allowed values: 'mpim', 'im', 'public_channel', 'private_channel'. Default: 'public_channel,private_channel'."),
			),
			mcp.WithNumber("limit",
				mcp.DefaultNumber(100),
				mcp.Description("Maximum number of items to return (1-999)."),
			),
			mcp.WithString("cursor",
				mcp.Description("Cursor for pagination."),
			),
		), channelsHandler.ChannelsMeHandler)
	}

	// User groups tools
	if cfg.ToolEnabled(ToolUsergroupsList) {
		s.AddTool(mcp.NewTool(ToolUsergroupsList,
			mcp.WithDescription("List all user groups (subteams) in the Slack workspace. User groups are mention groups like @engineering or @design that notify all members. Use this to discover available groups, check group membership counts, or find a group's ID before joining/updating it. Returns CSV with columns: id, name, handle, description, user_count, is_external."),
			mcp.WithTitleAnnotation("List User Groups"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithBoolean("include_users",
				mcp.Description("Include list of user IDs in each group. Default is false."),
				mcp.DefaultBool(false),
			),
			mcp.WithBoolean("include_count",
				mcp.Description("Include user count for each group. Default is true."),
				mcp.DefaultBool(true),
			),
			mcp.WithBoolean("include_disabled",
				mcp.Description("Include disabled/archived groups. Default is false."),
				mcp.DefaultBool(false),
			),
		), usergroupsHandler.UsergroupsListHandler)
	}

	if cfg.ToolEnabled(ToolUsergroupsMe) {
		s.AddTool(mcp.NewTool(ToolUsergroupsMe,
			mcp.WithDescription("Manage your own user group membership. Use action='list' to see which groups you belong to. Use action='join' with a usergroup_id to add yourself to a group (e.g., to receive @mentions). Use action='leave' with a usergroup_id to remove yourself. This is the easiest way to join/leave groups without needing to know the full member list. The join and leave actions are disabled unless SLACK_MCP_USERGROUPS_WRITE_TOOL is enabled."),
			mcp.WithTitleAnnotation("My User Groups"),
			mcp.WithString("action",
				mcp.Required(),
				mcp.Description("Action to perform: 'list' returns CSV of groups you're a member of, 'join' adds you to a group, 'leave' removes you from a group."),
			),
			mcp.WithString("usergroup_id",
				mcp.Description("ID of the user group (starts with 'S', e.g., 'S0123456789'). Required for 'join' and 'leave' actions. Get IDs from usergroups_list."),
			),
		), usergroupsHandler.UsergroupsMeHandler)
	}

	if cfg.ToolEnabled(ToolUsergroupsCreate) {
		s.AddTool(mcp.NewTool(ToolUsergroupsCreate,
			mcp.WithDescription("Create a new user group (mention group) in the Slack workspace. After creation, use usergroups_users_update to add members, or users can join themselves with usergroups_me. The handle becomes the @mention (e.g., handle='engineering' creates @engineering)."),
			mcp.WithTitleAnnotation("Create User Group"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("Display name of the user group (e.g., 'Engineering Team', 'Design Squad')."),
			),
			mcp.WithString("handle",
				mcp.Description("The @mention handle without the @ symbol (e.g., 'engineering' for @engineering). Keep it short and lowercase. If omitted, Slack auto-generates one from the name."),
			),
			mcp.WithString("description",
				mcp.Description("Purpose or description shown in group details (e.g., 'Backend and frontend engineers')."),
			),
			mcp.WithString("channels",
				mcp.Description("Comma-separated channel IDs where this group is commonly mentioned. Members get suggestions to join these channels."),
			),
		), usergroupsHandler.UsergroupsCreateHandler)
	}

	if cfg.ToolEnabled(ToolUsergroupsUpdate) {
		s.AddTool(mcp.NewTool(ToolUsergroupsUpdate,
			mcp.WithDescription("Update a user group's metadata: name, handle (@mention), description, or default channels. Does NOT change members - use usergroups_users_update for that. At least one field must be provided."),
			mcp.WithTitleAnnotation("Update User Group"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("usergroup_id",
				mcp.Required(),
				mcp.Description("ID of the user group to update (starts with 'S', e.g., 'S0123456789'). Get IDs from usergroups_list."),
			),
			mcp.WithString("name",
				mcp.Description("New display name for the group."),
			),
			mcp.WithString("handle",
				mcp.Description("New @mention handle (without @). Changing this changes how users mention the group."),
			),
			mcp.WithString("description",
				mcp.Description("New description for the group."),
			),
			mcp.WithString("channels",
				mcp.Description("New default channel IDs (comma-separated). Replaces existing default channels."),
			),
		), usergroupsHandler.UsergroupsUpdateHandler)
	}

	if cfg.ToolEnabled(ToolUsergroupsUsersUpdate) {
		s.AddTool(mcp.NewTool(ToolUsergroupsUsersUpdate,
			mcp.WithDescription("Replace all members of a user group with a new list. WARNING: This completely replaces the member list - any user not in the 'users' parameter will be removed. To add/remove just yourself, use usergroups_me instead. To add a single user without removing others, first get current members from usergroups_list with include_users=true, then call this with the combined list."),
			mcp.WithTitleAnnotation("Update User Group Members"),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithString("usergroup_id",
				mcp.Required(),
				mcp.Description("ID of the user group (starts with 'S', e.g., 'S0123456789'). Get IDs from usergroups_list."),
			),
			mcp.WithString("users",
				mcp.Required(),
				mcp.Description("Comma-separated user IDs that will become the COMPLETE member list (e.g., 'U0123456789,U9876543210'). All current members not in this list will be removed."),
			),
		), usergroupsHandler.UsergroupsUsersUpdateHandler)
	}

	logger.Info("Authenticating with Slack API...",
		zap.String("context", "console"),
	)
	ar, err := provider.Slack().AuthTest()
	if err != nil {
		logger.Fatal("Failed to authenticate with Slack",
			zap.String("context", "console"),
			zap.Error(err),
		)
	}

	logger.Info("Successfully authenticated with Slack",
		zap.String("context", "console"),
		zap.String("team", ar.Team),
		zap.String("user", ar.User),
		zap.String("enterprise", ar.EnterpriseID),
		zap.String("url", ar.URL),
	)

	ws, err := text.Workspace(ar.URL)
	if err != nil {
		logger.Fatal("Failed to parse workspace from URL",
			zap.String("context", "console"),
			zap.String("url", ar.URL),
			zap.Error(err),
		)
	}

	s.AddResource(mcp.NewResource(
		"slack://"+ws+"/channels",
		"Directory of Slack channels",
		mcp.WithResourceDescription("This resource provides a directory of Slack channels."),
		mcp.WithMIMEType("text/csv"),
	), channelsHandler.ChannelsResource)

	s.AddResource(mcp.NewResource(
		"slack://"+ws+"/users",
		"Directory of Slack users",
		mcp.WithResourceDescription("This resource provides a directory of Slack users."),
		mcp.WithMIMEType("text/csv"),
	), conversationsHandler.UsersResource)

	return &MCPServer{
		server: s,
		logger: logger,
	}
}

// httpEndpointPath is where the streamable HTTP transport is served.
const httpEndpointPath = "/mcp"

// ServeSSE builds the SSE transport for a server bound to host:port. The
// message endpoint is advertised as an absolute URL on the bind address, or
// as a relative path when bound to a wildcard address (0.0.0.0, ::) so
// clients resolve it against the address they connected to. Use
// ListenAndServeSSE to serve it behind HTTPSecurity.
func (s *MCPServer) ServeSSE(host, port string) *server.SSEServer {
	addr := net.JoinHostPort(host, port)
	s.logger.Info("Creating SSE server",
		zap.String("context", "console"),
		zap.String("version", version.Version),
		zap.String("build_time", version.BuildTime),
		zap.String("commit_hash", version.CommitHash),
		zap.String("address", addr),
	)
	return server.NewSSEServer(s.server,
		server.WithBaseURL("http://"+addr),
		server.WithUseFullURLForMessageEndpoint(!isWildcardHost(host)),
		server.WithSSEContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			ctx = auth.AuthFromRequest(s.logger)(ctx, r)

			return ctx
		}),
	)
}

func (s *MCPServer) ServeHTTP(addr string) *server.StreamableHTTPServer {
	s.logger.Info("Creating HTTP server",
		zap.String("context", "console"),
		zap.String("version", version.Version),
		zap.String("build_time", version.BuildTime),
		zap.String("commit_hash", version.CommitHash),
		zap.String("address", addr),
	)
	return server.NewStreamableHTTPServer(s.server,
		server.WithEndpointPath(httpEndpointPath),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			ctx = auth.AuthFromRequest(s.logger)(ctx, r)

			return ctx
		}),
	)
}

func (s *MCPServer) ServeStdio() error {
	s.logger.Info("Starting STDIO server",
		zap.String("version", version.Version),
		zap.String("build_time", version.BuildTime),
		zap.String("commit_hash", version.CommitHash),
	)
	err := server.ServeStdio(s.server)
	if err != nil {
		s.logger.Error("STDIO server error", zap.Error(err))
	}
	return err
}

// toolMiddlewares returns the tool middleware chain. Middleware runs in the
// order given: errors become tool results, then authentication, then
// logging, so unauthenticated calls are never logged with their parameters.
func toolMiddlewares(transport string, logger *zap.Logger) []server.ServerOption {
	return []server.ServerOption{
		server.WithToolHandlerMiddleware(buildErrorRecoveryMiddleware(logger)),
		server.WithToolHandlerMiddleware(auth.BuildMiddleware(transport, logger)),
		server.WithToolHandlerMiddleware(buildLoggerMiddleware(logger)),
	}
}

// buildErrorRecoveryMiddleware converts tool handler errors into MCP tool results
// with isError=true, allowing LLMs to see the error and retry with different parameters.
// Without this, errors become JSON-RPC -32603 protocol errors that crash MCP clients.
func buildErrorRecoveryMiddleware(logger *zap.Logger) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := next(ctx, req)
			if err != nil {
				logger.Warn("Tool call returned error, converting to isError tool result",
					zap.String("tool", req.Params.Name),
					zap.Error(err),
				)
				return mcp.NewToolResultError(err.Error()), nil
			}
			return res, nil
		}
	}
}

func buildLoggerMiddleware(logger *zap.Logger) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			logger.Info("Request received",
				zap.String("tool", req.Params.Name),
				zap.Any("params", redactedToolParams(req)),
			)
			logger.Debug("Request parameters",
				zap.String("tool", req.Params.Name),
				zap.Any("params", loggableToolParams(req)),
			)

			startTime := time.Now()

			res, err := next(ctx, req)

			duration := time.Since(startTime)

			logger.Info("Request finished",
				zap.String("tool", req.Params.Name),
				zap.Duration("duration", duration),
			)

			return res, err
		}
	}
}

// redactedArguments are tool arguments that carry message or file content.
// They are replaced by their size in Info-level logs.
var redactedArguments = map[string]bool{
	"text":            true,
	"payload":         true,
	"blocks":          true,
	"content":         true,
	"content_base64":  true,
	"initial_comment": true,
	"topic":           true,
	"purpose":         true,
	"description":     true,
	"search_query":    true,
	"query":           true,
}

// redactedToolParams returns the tool arguments with message text, blocks
// and file content replaced by a size marker, for Info-level logging.
func redactedToolParams(req mcp.CallToolRequest) map[string]any {
	args := req.GetArguments()
	out := make(map[string]any, len(args))
	for k, v := range args {
		if !redactedArguments[k] {
			out[k] = v
			continue
		}
		size := 0
		switch val := v.(type) {
		case string:
			size = len(val)
		case nil:
		default:
			if b, err := json.Marshal(val); err == nil {
				size = len(b)
			}
		}
		out[k] = fmt.Sprintf("[redacted %d bytes]", size)
	}
	return out
}

// loggableToolParams returns the parameters logged at Debug level. File
// upload content is never logged.
func loggableToolParams(req mcp.CallToolRequest) any {
	if req.Params.Name != ToolFilesUpload {
		return req.Params
	}

	args := req.GetArguments()
	contentBytes := 0
	if content, ok := args["content"].(string); ok {
		contentBytes += len(content)
	}
	if encoded, ok := args["content_base64"].(string); ok {
		contentBytes += base64.StdEncoding.DecodedLen(len(encoded))
	}
	return map[string]any{
		"channel_id":               args["channel_id"],
		"filename":                 args["filename"],
		"content_bytes":            contentBytes,
		"content_redacted":         true,
		"initial_comment_redacted": args["initial_comment"] != nil,
	}
}
