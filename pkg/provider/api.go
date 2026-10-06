package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/limiter"
	"github.com/bdpdx/slack-mcp-server/pkg/transport"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

const usersNotReadyMsg = "users cache is not ready yet, sync process is still running... please wait"
const channelsNotReadyMsg = "channels cache is not ready yet, sync process is still running... please wait"
const defaultCacheTTL = 24 * time.Hour
const defaultMinRefreshInterval = 30 * time.Second

var AllChanTypes = []string{"mpim", "im", "public_channel", "private_channel"}
var PrivateChanType = "private_channel"
var PubChanType = "public_channel"

var ErrUsersNotReady = errors.New(usersNotReadyMsg)
var ErrChannelsNotReady = errors.New(channelsNotReadyMsg)
var ErrRefreshRateLimited = errors.New("refresh skipped due to rate limiting")

// atomicWriteFile writes data to a file atomically using a temp file and rename.
// Uses os.CreateTemp for unpredictable temp file names (prevents symlink attacks)
// and cleans up the temp file on rename failure.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cache_*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("setting file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// getCacheDir returns the cache directory for slack-mcp-server, creating it
// with owner-only permissions. It never falls back to the working directory:
// if the user cache dir is unavailable the caller must fail.
func getCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating user cache dir: %w", err)
	}

	dir := filepath.Join(cacheDir, "slack-mcp-server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating cache dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone; tighten it.
	if err := os.Chmod(dir, 0700); err != nil {
		return "", fmt.Errorf("setting cache dir permissions: %w", err)
	}
	return dir, nil
}

// getCacheTTL returns the cache TTL from SLACK_MCP_CACHE_TTL env var or default (24 hours).
// Supports formats: "1h", "30m", "3600" (seconds), "0" (disable TTL, cache forever)
// Negative values are rejected and fall back to default.
func getCacheTTL() time.Duration {
	ttlStr := os.Getenv("SLACK_MCP_CACHE_TTL")
	if ttlStr == "" {
		return defaultCacheTTL
	}

	// Try parsing as duration first (e.g., "1h", "30m")
	if d, err := time.ParseDuration(ttlStr); err == nil {
		if d < 0 {
			return defaultCacheTTL // Reject negative TTL
		}
		return d
	}

	// Try parsing as seconds (e.g., "3600")
	if secs, err := strconv.ParseInt(ttlStr, 10, 64); err == nil {
		if secs < 0 {
			return defaultCacheTTL // Reject negative TTL
		}
		return time.Duration(secs) * time.Second
	}

	return defaultCacheTTL
}

// getMinRefreshInterval returns the minimum interval between forced refreshes from
// SLACK_MCP_MIN_REFRESH_INTERVAL env var or default (30s).
// Supports formats: "30s", "1m", "60" (seconds), "0" (disable rate limiting)
// Negative values are rejected and fall back to default.
func getMinRefreshInterval() time.Duration {
	intervalStr := os.Getenv("SLACK_MCP_MIN_REFRESH_INTERVAL")
	if intervalStr == "" {
		return defaultMinRefreshInterval
	}

	// Try parsing as duration first (e.g., "30s", "1m")
	if d, err := time.ParseDuration(intervalStr); err == nil {
		if d < 0 {
			return defaultMinRefreshInterval // Reject negative interval
		}
		return d
	}

	// Try parsing as seconds (e.g., "60")
	if secs, err := strconv.ParseInt(intervalStr, 10, 64); err == nil {
		if secs < 0 {
			return defaultMinRefreshInterval // Reject negative interval
		}
		return time.Duration(secs) * time.Second
	}

	return defaultMinRefreshInterval
}

// teamIDPattern matches Slack team (T...) and enterprise (E...) IDs.
var teamIDPattern = regexp.MustCompile(`^[TE][A-Z0-9]{2,}$`)

// getCachePathWithTeamID returns a cache file path prefixed with TeamID for
// workspace isolation. The TeamID comes from auth.test and is validated before
// it is used as part of a file name.
func getCachePathWithTeamID(teamID, filename string) (string, error) {
	if !teamIDPattern.MatchString(teamID) {
		return "", fmt.Errorf("unexpected team ID %q from auth.test", teamID)
	}
	cacheDir, err := getCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, teamID+"_"+filename), nil
}

type UsersCache struct {
	Users    map[string]slack.User `json:"users"`
	UsersInv map[string]string     `json:"users_inv"`
}

type ChannelsCache struct {
	Channels    map[string]Channel `json:"channels"`
	ChannelsInv map[string]string  `json:"channels_inv"`
}

type Channel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Topic       string   `json:"topic"`
	Purpose     string   `json:"purpose"`
	MemberCount int      `json:"memberCount"`
	IsMpIM      bool     `json:"mpim"`
	IsIM        bool     `json:"im"`
	IsPrivate   bool     `json:"private"`
	IsExtShared bool     `json:"is_ext_shared"`     // Shared with external organizations
	User        string   `json:"user,omitempty"`    // User ID for IM channels
	Members     []string `json:"members,omitempty"` // Member IDs for the channel
}

type SlackAPI interface {
	// Standard slack-go API methods
	AuthTest() (*slack.AuthTestResponse, error)
	AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error)
	GetUsersContext(ctx context.Context, options ...slack.GetUsersOption) ([]slack.User, error)
	GetUsersInfo(users ...string) (*[]slack.User, error)
	PostMessageContext(ctx context.Context, channel string, options ...slack.MsgOption) (string, string, error)
	DeleteMessageContext(ctx context.Context, channel, msgTimestamp string) (string, string, error)
	OpenConversationContext(ctx context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error)
	MarkConversationContext(ctx context.Context, channel, ts string) error
	AddReactionContext(ctx context.Context, name string, item slack.ItemRef) error
	RemoveReactionContext(ctx context.Context, name string, item slack.ItemRef) error
	LeaveConversationContext(ctx context.Context, channelID string) (bool, error)
	JoinConversationContext(ctx context.Context, channelID string) (*slack.Channel, string, []string, error)
	RenameConversationContext(ctx context.Context, channelID, channelName string) (*slack.Channel, error)
	SetTopicOfConversationContext(ctx context.Context, channelID, topic string) (*slack.Channel, error)
	SetPurposeOfConversationContext(ctx context.Context, channelID, purpose string) (*slack.Channel, error)
	CreateConversationContext(ctx context.Context, params slack.CreateConversationParams) (*slack.Channel, error)
	InviteUsersToConversationContext(ctx context.Context, channelID string, users ...string) (*slack.Channel, error)
	InviteSharedEmailsToConversationContext(ctx context.Context, channelID string, emails ...string) (string, bool, error)
	InviteSharedUserIDsToConversationContext(ctx context.Context, channelID string, userIDs ...string) (string, bool, error)

	// Used to get messages
	GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error)
	SearchContext(ctx context.Context, query string, params slack.SearchParameters) (*slack.SearchMessages, *slack.SearchFiles, error)

	// Used to get files
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error)
	GetFileContext(ctx context.Context, downloadURL string, writer io.Writer) error
	UploadFileContext(ctx context.Context, params slack.UploadFileParameters) (*slack.FileSummary, error)

	// Used to get channel info (for unread counts)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)

	// Used to get channels list from both Slack and Enterprise Grid versions
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)

	// Used to list only channels the calling user is a member of (users.conversations).
	// For xoxp tokens this is more efficient than conversations.list because it excludes
	// non-member public channels and closed DMs that cannot have unreads.
	GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error)

	// User groups API methods
	GetUserGroupsContext(ctx context.Context, options ...slack.GetUserGroupsOption) ([]slack.UserGroup, error)
	GetUserGroupMembersContext(ctx context.Context, userGroup string, options ...slack.GetUserGroupMembersOption) ([]string, error)
	CreateUserGroupContext(ctx context.Context, userGroup slack.UserGroup, options ...slack.CreateUserGroupOption) (slack.UserGroup, error)
	UpdateUserGroupContext(ctx context.Context, userGroupID string, options ...slack.UpdateUserGroupsOption) (slack.UserGroup, error)
	UpdateUserGroupMembersContext(ctx context.Context, userGroup string, members string, options ...slack.UpdateUserGroupMembersOption) (slack.UserGroup, error)
}

type MCPSlackClient struct {
	slackClient *slack.Client
	httpClient  *http.Client
	token       string

	authResponse *slack.AuthTestResponse

	isEnterprise bool
	isBotToken   bool
}

type ApiProvider struct {
	transport string
	client    SlackAPI
	// userClient is a user-token (xoxp) client used for calls that must act as
	// the user when a bot token is the primary client. nil when not configured.
	userClient SlackAPI
	logger     *zap.Logger

	rateLimiter        *rate.Limiter
	cacheTTL           time.Duration
	minRefreshInterval time.Duration

	// Users cache: atomic pointer to immutable snapshot (no copy on read)
	usersSnapshot          atomic.Pointer[UsersCache]
	usersCachePath         string
	usersReady             atomic.Bool
	refreshingUsers        atomic.Bool // true while a background refresh goroutine is running
	lastForcedUsersRefresh time.Time
	usersMu                sync.RWMutex // protects lastForcedUsersRefresh
	fetchUsersMu           sync.Mutex   // serializes fetchAndStoreUsers calls

	// Channels cache: atomic pointer to immutable snapshot (no copy on read)
	channelsSnapshot          atomic.Pointer[ChannelsCache]
	channelsCachePath         string
	channelsReady             atomic.Bool
	refreshingChannels        atomic.Bool // true while a background refresh goroutine is running
	lastForcedChannelsRefresh time.Time
	channelsMu                sync.RWMutex // protects lastForcedChannelsRefresh
	fetchChannelsMu           sync.Mutex   // serializes fetchAndStoreChannels calls
}

// isUserToken reports whether token is a Slack user OAuth token (xoxp),
// including its token-rotation variant.
func isUserToken(token string) bool {
	return strings.HasPrefix(token, "xoxp-") || strings.HasPrefix(token, "xoxe.xoxp-")
}

// isBotToken reports whether token is a Slack bot token (xoxb), including its
// token-rotation variant.
func isBotToken(token string) bool {
	return strings.HasPrefix(token, "xoxb-") || strings.HasPrefix(token, "xoxe.xoxb-")
}

func NewMCPSlackClient(token string, logger *zap.Logger) (*MCPSlackClient, error) {
	if !isUserToken(token) && !isBotToken(token) {
		return nil, errors.New("unsupported Slack token type: only bot (xoxb-) and user (xoxp-) tokens are supported")
	}

	httpClient := transport.ProvideHTTPClient(logger)

	slackOpts := []slack.Option{slack.OptionHTTPClient(httpClient)}
	if os.Getenv("SLACK_MCP_GOVSLACK") == "true" {
		slackOpts = append(slackOpts, slack.OptionAPIURL("https://slack-gov.com/api/"))
	}
	slackClient := slack.New(token, slackOpts...)

	authResp, err := slackClient.AuthTest()
	if err != nil {
		return nil, err
	}

	logger.Info("Authenticated to Slack",
		zap.String("team", authResp.Team),
		zap.String("team_id", authResp.TeamID),
		zap.String("user", authResp.User))

	authResponse := &slack.AuthTestResponse{
		URL:          authResp.URL,
		Team:         authResp.Team,
		User:         authResp.User,
		TeamID:       authResp.TeamID,
		UserID:       authResp.UserID,
		EnterpriseID: authResp.EnterpriseID,
		BotID:        authResp.BotID,
	}

	apiURL, err := teamAPIURL(authResp.URL)
	if err != nil {
		return nil, err
	}
	slackClient = slack.New(token,
		slack.OptionHTTPClient(httpClient),
		slack.OptionAPIURL(apiURL),
	)

	return &MCPSlackClient{
		slackClient:  slackClient,
		httpClient:   httpClient,
		token:        token,
		authResponse: authResponse,
		isEnterprise: authResp.EnterpriseID != "",
		isBotToken:   isBotToken(token),
	}, nil
}

// teamAPIURL derives the workspace API base URL from the auth.test URL and
// refuses anything that is not an https Slack host, so the token is never sent
// elsewhere.
func teamAPIURL(teamURL string) (string, error) {
	u, err := url.Parse(teamURL)
	if err != nil || u.Scheme != "https" || !transport.IsSlackHost(u.Hostname()) {
		return "", fmt.Errorf("unexpected workspace URL %q from auth.test", teamURL)
	}
	return "https://" + u.Host + "/api/", nil
}

func (c *MCPSlackClient) AuthTest() (*slack.AuthTestResponse, error) {
	if c.authResponse != nil {
		return c.authResponse, nil
	}

	return c.slackClient.AuthTest()
}

func (c *MCPSlackClient) AuthTestContext(ctx context.Context) (*slack.AuthTestResponse, error) {
	return c.slackClient.AuthTestContext(ctx)
}

func (c *MCPSlackClient) GetUsersContext(ctx context.Context, options ...slack.GetUsersOption) ([]slack.User, error) {
	return c.slackClient.GetUsersContext(ctx, options...)
}

func (c *MCPSlackClient) GetUsersInfo(users ...string) (*[]slack.User, error) {
	return c.slackClient.GetUsersInfo(users...)
}

func (c *MCPSlackClient) MarkConversationContext(ctx context.Context, channel, ts string) error {
	return c.slackClient.MarkConversationContext(ctx, channel, ts)
}

func (c *MCPSlackClient) LeaveConversationContext(ctx context.Context, channelID string) (bool, error) {
	return c.slackClient.LeaveConversationContext(ctx, channelID)
}

func (c *MCPSlackClient) JoinConversationContext(ctx context.Context, channelID string) (*slack.Channel, string, []string, error) {
	return c.slackClient.JoinConversationContext(ctx, channelID)
}

func (c *MCPSlackClient) RenameConversationContext(ctx context.Context, channelID, channelName string) (*slack.Channel, error) {
	return c.slackClient.RenameConversationContext(ctx, channelID, channelName)
}

func (c *MCPSlackClient) SetTopicOfConversationContext(ctx context.Context, channelID, topic string) (*slack.Channel, error) {
	return c.slackClient.SetTopicOfConversationContext(ctx, channelID, topic)
}

func (c *MCPSlackClient) SetPurposeOfConversationContext(ctx context.Context, channelID, purpose string) (*slack.Channel, error) {
	return c.slackClient.SetPurposeOfConversationContext(ctx, channelID, purpose)
}

func (c *MCPSlackClient) CreateConversationContext(ctx context.Context, params slack.CreateConversationParams) (*slack.Channel, error) {
	return c.slackClient.CreateConversationContext(ctx, params)
}

func (c *MCPSlackClient) InviteUsersToConversationContext(ctx context.Context, channelID string, users ...string) (*slack.Channel, error) {
	return c.slackClient.InviteUsersToConversationContext(ctx, channelID, users...)
}

func (c *MCPSlackClient) InviteSharedEmailsToConversationContext(ctx context.Context, channelID string, emails ...string) (string, bool, error) {
	return c.slackClient.InviteSharedEmailsToConversationContext(ctx, channelID, emails...)
}

func (c *MCPSlackClient) InviteSharedUserIDsToConversationContext(ctx context.Context, channelID string, userIDs ...string) (string, bool, error) {
	return c.slackClient.InviteSharedUserIDsToConversationContext(ctx, channelID, userIDs...)
}

func (c *MCPSlackClient) GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	return c.slackClient.GetConversationsContext(ctx, params)
}

func (c *MCPSlackClient) GetConversationsForUserContext(ctx context.Context, params *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	return c.slackClient.GetConversationsForUserContext(ctx, params)
}

func (c *MCPSlackClient) GetConversationHistoryContext(ctx context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	return c.slackClient.GetConversationHistoryContext(ctx, params)
}

func (c *MCPSlackClient) GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) (msgs []slack.Message, hasMore bool, nextCursor string, err error) {
	return c.slackClient.GetConversationRepliesContext(ctx, params)
}

func (c *MCPSlackClient) SearchContext(ctx context.Context, query string, params slack.SearchParameters) (*slack.SearchMessages, *slack.SearchFiles, error) {
	return c.slackClient.SearchContext(ctx, query, params)
}

func (c *MCPSlackClient) PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	return c.slackClient.PostMessageContext(ctx, channelID, options...)
}

func (c *MCPSlackClient) DeleteMessageContext(ctx context.Context, channel, msgTimestamp string) (string, string, error) {
	return c.slackClient.DeleteMessageContext(ctx, channel, msgTimestamp)
}

func (c *MCPSlackClient) OpenConversationContext(ctx context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	return c.slackClient.OpenConversationContext(ctx, params)
}

func (c *MCPSlackClient) AddReactionContext(ctx context.Context, name string, item slack.ItemRef) error {
	return c.slackClient.AddReactionContext(ctx, name, item)
}

func (c *MCPSlackClient) RemoveReactionContext(ctx context.Context, name string, item slack.ItemRef) error {
	return c.slackClient.RemoveReactionContext(ctx, name, item)
}

func (c *MCPSlackClient) GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error) {
	return c.slackClient.GetFileInfoContext(ctx, fileID, count, page)
}

// GetFileContext downloads a Slack-hosted file into writer. It refuses URLs
// that are not https Slack file hosts and stops after MaxFileDownloadBytes.
func (c *MCPSlackClient) GetFileContext(ctx context.Context, downloadURL string, writer io.Writer) error {
	return downloadFile(ctx, c.httpClient, c.token, downloadURL, writer, MaxFileDownloadBytes)
}

func (c *MCPSlackClient) UploadFileContext(ctx context.Context, params slack.UploadFileParameters) (*slack.FileSummary, error) {
	return c.slackClient.UploadFileContext(ctx, params)
}

func (c *MCPSlackClient) GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error) {
	return c.slackClient.GetConversationInfoContext(ctx, input)
}

func (c *MCPSlackClient) GetUserGroupsContext(ctx context.Context, options ...slack.GetUserGroupsOption) ([]slack.UserGroup, error) {
	return c.slackClient.GetUserGroupsContext(ctx, options...)
}

func (c *MCPSlackClient) GetUserGroupMembersContext(ctx context.Context, userGroup string, options ...slack.GetUserGroupMembersOption) ([]string, error) {
	return c.slackClient.GetUserGroupMembersContext(ctx, userGroup, options...)
}

func (c *MCPSlackClient) CreateUserGroupContext(ctx context.Context, userGroup slack.UserGroup, options ...slack.CreateUserGroupOption) (slack.UserGroup, error) {
	return c.slackClient.CreateUserGroupContext(ctx, userGroup, options...)
}

func (c *MCPSlackClient) UpdateUserGroupContext(ctx context.Context, userGroupID string, options ...slack.UpdateUserGroupsOption) (slack.UserGroup, error) {
	return c.slackClient.UpdateUserGroupContext(ctx, userGroupID, options...)
}

func (c *MCPSlackClient) UpdateUserGroupMembersContext(ctx context.Context, userGroup string, members string, options ...slack.UpdateUserGroupMembersOption) (slack.UserGroup, error) {
	return c.slackClient.UpdateUserGroupMembersContext(ctx, userGroup, members, options...)
}

func (c *MCPSlackClient) IsEnterprise() bool {
	return c.isEnterprise
}

func (c *MCPSlackClient) AuthResponse() *slack.AuthTestResponse {
	return c.authResponse
}

func (c *MCPSlackClient) IsBotToken() bool {
	return c.isBotToken
}

func (c *MCPSlackClient) Raw() *slack.Client {
	return c.slackClient
}

// New builds the provider from SLACK_MCP_XOXB_TOKEN and/or
// SLACK_MCP_XOXP_TOKEN. Only Slack app tokens are supported.
func New(transport string, logger *zap.Logger) *ApiProvider {
	xoxpToken := os.Getenv("SLACK_MCP_XOXP_TOKEN")
	xoxbToken := os.Getenv("SLACK_MCP_XOXB_TOKEN")

	if xoxbToken == "" && xoxpToken == "" {
		logger.Fatal("Authentication required: set SLACK_MCP_XOXB_TOKEN (bot token, xoxb-) and/or SLACK_MCP_XOXP_TOKEN (user token, xoxp-)")
	}
	if xoxbToken != "" && !isBotToken(xoxbToken) {
		logger.Fatal("SLACK_MCP_XOXB_TOKEN must be a bot token starting with xoxb-")
	}
	if xoxpToken != "" && !isUserToken(xoxpToken) {
		logger.Fatal("SLACK_MCP_XOXP_TOKEN must be a user token starting with xoxp-")
	}

	// Priority 1: XOXB token (Bot). If a user token is also set, keep a
	// separate user client for the calls that must act as the user.
	if xoxbToken != "" {
		logger.Info("Using Bot token authentication",
			zap.String("context", "console"),
			zap.String("token_type", "xoxb"),
		)

		ap := newWithToken(transport, xoxbToken, logger)

		if xoxpToken != "" {
			userClient, err := NewMCPSlackClient(xoxpToken, logger)
			if err != nil {
				logger.Fatal("Failed to create MCP Slack user client", zap.Error(err))
			}
			ap.userClient = userClient

			logger.Info("Both SLACK_MCP_XOXB_TOKEN and SLACK_MCP_XOXP_TOKEN are set. "+
				"Using Bot token by default and User token for channel management, DM, and delete-message tools.",
				zap.String("context", "console"),
			)
		}

		return ap
	}

	// Priority 2: XOXP token (User OAuth)
	return newWithToken(transport, xoxpToken, logger)
}

func newWithToken(transport string, token string, logger *zap.Logger) *ApiProvider {
	client, err := NewMCPSlackClient(token, logger)
	if err != nil {
		logger.Fatal("Authentication failed - check your Slack tokens", zap.Error(err))
	}
	teamID := client.AuthResponse().TeamID

	usersCache := os.Getenv("SLACK_MCP_USERS_CACHE")
	if usersCache == "" {
		usersCache, err = getCachePathWithTeamID(teamID, "users_cache.json")
		if err != nil {
			logger.Fatal("Failed to determine users cache path (set SLACK_MCP_USERS_CACHE)", zap.Error(err))
		}
	}

	channelsCache := os.Getenv("SLACK_MCP_CHANNELS_CACHE")
	if channelsCache == "" {
		channelsCache, err = getCachePathWithTeamID(teamID, "channels_cache_v2.json")
		if err != nil {
			logger.Fatal("Failed to determine channels cache path (set SLACK_MCP_CHANNELS_CACHE)", zap.Error(err))
		}
	}

	ap := &ApiProvider{
		transport: transport,
		client:    client,
		logger:    logger,

		rateLimiter:        limiter.Tier2.Limiter(),
		cacheTTL:           getCacheTTL(),
		minRefreshInterval: getMinRefreshInterval(),

		usersCachePath:    usersCache,
		channelsCachePath: channelsCache,
	}
	// Initialize with empty snapshots
	ap.usersSnapshot.Store(&UsersCache{
		Users:    make(map[string]slack.User),
		UsersInv: make(map[string]string),
	})
	ap.channelsSnapshot.Store(&ChannelsCache{
		Channels:    make(map[string]Channel),
		ChannelsInv: make(map[string]string),
	})
	return ap
}

func (ap *ApiProvider) RefreshUsers(ctx context.Context) error {
	return ap.refreshUsersInternal(ctx, false)
}

// ForceRefreshUsers bypasses the cache and fetches fresh user data from Slack API.
// Rate limited by SLACK_MCP_MIN_REFRESH_INTERVAL (default 30s) to prevent API abuse.
// Returns ErrRefreshRateLimited if refresh is skipped due to rate limiting.
func (ap *ApiProvider) ForceRefreshUsers(ctx context.Context) error {
	if ap.minRefreshInterval > 0 {
		// Use single lock scope for check-and-update to prevent TOCTOU race
		ap.usersMu.Lock()
		sinceLast := time.Since(ap.lastForcedUsersRefresh)
		if sinceLast < ap.minRefreshInterval {
			ap.usersMu.Unlock()
			ap.logger.Debug("Skipping forced users refresh, within rate limit",
				zap.Duration("since_last", sinceLast),
				zap.Duration("min_interval", ap.minRefreshInterval))
			return ErrRefreshRateLimited
		}
		// Update timestamp before refresh to prevent concurrent forced refreshes
		ap.lastForcedUsersRefresh = time.Now()
		ap.usersMu.Unlock()
	}

	ap.logger.Info("Force refreshing users cache")
	return ap.refreshUsersInternal(ctx, true)
}

// PatchUser fetches a single user by ID from the Slack API and adds them to
// the in-memory users snapshot. This is much cheaper than a full cache rebuild
// for a single cache miss (O(1) API call vs O(all users)).
// Disk persistence is skipped — the next full refresh will persist the entry.
// The load-merge-store is serialized with fetchAndStoreUsers so a concurrent
// full refresh cannot be overwritten by a snapshot built from stale data.
func (ap *ApiProvider) PatchUser(ctx context.Context, userID string) (*slack.User, error) {
	usersInfo, err := ap.client.GetUsersInfo(userID)
	if err != nil {
		ap.logger.Warn("Failed to fetch user for cache patch", zap.String("user_id", userID), zap.Error(err))
		return nil, err
	}
	if usersInfo == nil || len(*usersInfo) == 0 {
		ap.logger.Debug("User not found via API", zap.String("user_id", userID))
		return nil, errors.New("user not found")
	}

	user := (*usersInfo)[0]

	ap.fetchUsersMu.Lock()
	defer ap.fetchUsersMu.Unlock()

	current := ap.usersSnapshot.Load()

	newSnapshot := &UsersCache{
		Users:    make(map[string]slack.User, len(current.Users)+1),
		UsersInv: make(map[string]string, len(current.UsersInv)+1),
	}
	for k, v := range current.Users {
		newSnapshot.Users[k] = v
	}
	for k, v := range current.UsersInv {
		newSnapshot.UsersInv[k] = v
	}
	newSnapshot.Users[user.ID] = user
	newSnapshot.UsersInv[user.Name] = user.ID

	ap.usersSnapshot.Store(newSnapshot)
	ap.logger.Debug("Patched user into cache",
		zap.String("user_id", user.ID),
		zap.String("user_name", user.Name))

	return &user, nil
}

func (ap *ApiProvider) refreshUsersInternal(ctx context.Context, force bool) error {
	ap.usersMu.Lock()

	// Check if we should use cache (not forced, cache exists)
	if !force {
		if data, err := os.ReadFile(ap.usersCachePath); err == nil {
			var cachedUsers []slack.User
			if err := json.Unmarshal(data, &cachedUsers); err != nil {
				ap.logger.Warn("Failed to unmarshal users cache, will refetch",
					zap.String("cache_file", ap.usersCachePath),
					zap.Error(err))
			} else if len(cachedUsers) == 0 {
				ap.logger.Warn("Users cache is empty or null, will refetch",
					zap.String("cache_file", ap.usersCachePath))
			} else {
				// Build snapshot from cache
				newSnapshot := &UsersCache{
					Users:    make(map[string]slack.User, len(cachedUsers)),
					UsersInv: make(map[string]string, len(cachedUsers)),
				}
				for _, u := range cachedUsers {
					newSnapshot.Users[u.ID] = u
					newSnapshot.UsersInv[u.Name] = u.ID
				}
				ap.usersSnapshot.Store(newSnapshot)
				ap.usersReady.Store(true)

				// Check cache TTL using file modification time
				cacheExpired := false
				if ap.cacheTTL > 0 {
					if fileInfo, err := os.Stat(ap.usersCachePath); err == nil {
						cacheAge := time.Since(fileInfo.ModTime())
						if cacheAge > ap.cacheTTL {
							cacheExpired = true
							ap.logger.Info("Serving stale users cache, background refresh starting",
								zap.Duration("cache_age", cacheAge),
								zap.Duration("ttl", ap.cacheTTL),
								zap.Int("count", len(cachedUsers)),
								zap.String("cache_file", ap.usersCachePath))
						}
					}
				}

				if !cacheExpired {
					ap.logger.Info("Loaded users from cache",
						zap.Int("count", len(cachedUsers)),
						zap.String("cache_file", ap.usersCachePath))
					ap.usersMu.Unlock()
					return nil
				}

				// Cache is expired: release lock, spawn background refresh, return immediately
				ap.usersMu.Unlock()
				ap.spawnBackgroundUsersRefresh()
				return nil
			}
		}
	}

	// No usable cache: fetch fresh data synchronously (first run or force)
	ap.usersMu.Unlock()
	return ap.fetchAndStoreUsers(ctx)
}

// spawnBackgroundUsersRefresh starts a background goroutine to fetch fresh user data.
// Uses refreshingUsers flag to prevent concurrent background refreshes.
func (ap *ApiProvider) spawnBackgroundUsersRefresh() {
	if !ap.refreshingUsers.CompareAndSwap(false, true) {
		ap.logger.Debug("Skipping background users refresh, already in progress")
		return
	}
	go func() {
		defer ap.refreshingUsers.Store(false)
		if err := ap.fetchAndStoreUsers(context.Background()); err != nil {
			ap.logger.Warn("Background users refresh failed, continuing with stale data",
				zap.Error(err))
		}
	}()
}

// fetchAndStoreUsers fetches all users from the Slack API and updates the snapshot and cache file.
// Serialized by fetchUsersMu to prevent concurrent fetches from racing on snapshot/file writes.
func (ap *ApiProvider) fetchAndStoreUsers(ctx context.Context) error {
	ap.fetchUsersMu.Lock()
	defer ap.fetchUsersMu.Unlock()

	users, err := ap.client.GetUsersContext(ctx,
		slack.GetUsersOptionLimit(1000),
	)
	if err != nil {
		ap.logger.Error("Failed to fetch users", zap.Error(err))
		return err
	}

	if len(users) == 0 {
		if ap.usersReady.Load() {
			ap.logger.Warn("API returned zero users, keeping existing cache")
			return nil
		}
		return errors.New("API returned zero users and no existing cache is available")
	}

	// Build new snapshot
	newSnapshot := &UsersCache{
		Users:    make(map[string]slack.User, len(users)),
		UsersInv: make(map[string]string, len(users)),
	}
	for _, user := range users {
		newSnapshot.Users[user.ID] = user
		newSnapshot.UsersInv[user.Name] = user.ID
	}
	ap.usersSnapshot.Store(newSnapshot)

	if data, err := json.MarshalIndent(users, "", "  "); err != nil {
		ap.logger.Error("Failed to marshal users for cache", zap.Error(err))
	} else {
		// Atomic write: temp file + rename to prevent partial/corrupt files
		if err := atomicWriteFile(ap.usersCachePath, data, 0600); err != nil {
			ap.logger.Error("Failed to write cache file",
				zap.String("cache_file", ap.usersCachePath),
				zap.Error(err))
		} else {
			ap.logger.Info("Wrote users to cache",
				zap.Int("count", len(users)),
				zap.String("cache_file", ap.usersCachePath))
		}
	}

	ap.usersReady.Store(true)

	return nil
}

func (ap *ApiProvider) RefreshChannels(ctx context.Context) error {
	return ap.refreshChannelsInternal(ctx, false)
}

// ForceRefreshChannels bypasses the cache and fetches fresh channel data from Slack API.
// Use this when a channel lookup fails to attempt recovery with fresh data.
// Rate limited by SLACK_MCP_MIN_REFRESH_INTERVAL (default 30s) to prevent API abuse.
// Returns ErrRefreshRateLimited if refresh is skipped due to rate limiting.
func (ap *ApiProvider) ForceRefreshChannels(ctx context.Context) error {
	if ap.minRefreshInterval > 0 {
		// Use single lock scope for check-and-update to prevent TOCTOU race
		ap.channelsMu.Lock()
		sinceLast := time.Since(ap.lastForcedChannelsRefresh)
		if sinceLast < ap.minRefreshInterval {
			ap.channelsMu.Unlock()
			ap.logger.Debug("Skipping forced channels refresh, within rate limit",
				zap.Duration("since_last", sinceLast),
				zap.Duration("min_interval", ap.minRefreshInterval))
			return ErrRefreshRateLimited
		}
		// Update timestamp before refresh to prevent concurrent forced refreshes
		ap.lastForcedChannelsRefresh = time.Now()
		ap.channelsMu.Unlock()
	}

	ap.logger.Info("Force refreshing channels cache")
	return ap.refreshChannelsInternal(ctx, true)
}

func (ap *ApiProvider) refreshChannelsInternal(ctx context.Context, force bool) error {
	ap.channelsMu.Lock()

	// Check if we should use cache (not forced, cache exists)
	if !force {
		if data, err := os.ReadFile(ap.channelsCachePath); err == nil {
			var cachedChannels []Channel
			if err := json.Unmarshal(data, &cachedChannels); err != nil {
				ap.logger.Warn("Failed to unmarshal channels cache, will refetch",
					zap.String("cache_file", ap.channelsCachePath),
					zap.Error(err))
			} else if len(cachedChannels) == 0 {
				ap.logger.Warn("Channels cache is empty or null, will refetch",
					zap.String("cache_file", ap.channelsCachePath))
			} else {
				// Re-map channels with current users cache to ensure DM names are populated
				usersMap := ap.ProvideUsersMap().Users
				newSnapshot := &ChannelsCache{
					Channels:    make(map[string]Channel, len(cachedChannels)),
					ChannelsInv: make(map[string]string, len(cachedChannels)),
				}
				for _, c := range cachedChannels {
					if c.IsIM {
						remappedChannel := mapChannel(
							c.ID, "", "", c.Topic, c.Purpose,
							c.User, c.Members, c.MemberCount,
							c.IsIM, c.IsMpIM, c.IsPrivate, c.IsExtShared,
							usersMap,
						)
						newSnapshot.Channels[c.ID] = remappedChannel
						newSnapshot.ChannelsInv[remappedChannel.Name] = c.ID
					} else {
						newSnapshot.Channels[c.ID] = c
						newSnapshot.ChannelsInv[c.Name] = c.ID
					}
				}
				ap.channelsSnapshot.Store(newSnapshot)
				ap.channelsReady.Store(true)

				// Check cache TTL using file modification time
				cacheExpired := false
				if ap.cacheTTL > 0 {
					if fileInfo, err := os.Stat(ap.channelsCachePath); err == nil {
						cacheAge := time.Since(fileInfo.ModTime())
						if cacheAge > ap.cacheTTL {
							cacheExpired = true
							ap.logger.Info("Serving stale channels cache, background refresh starting",
								zap.Duration("cache_age", cacheAge),
								zap.Duration("ttl", ap.cacheTTL),
								zap.Int("count", len(cachedChannels)),
								zap.String("cache_file", ap.channelsCachePath))
						}
					}
				}

				if !cacheExpired {
					ap.logger.Info("Loaded channels from cache and re-mapped DM names",
						zap.Int("count", len(cachedChannels)),
						zap.String("cache_file", ap.channelsCachePath))
					ap.channelsMu.Unlock()
					return nil
				}

				// Cache is expired: release lock, spawn background refresh, return immediately
				ap.channelsMu.Unlock()
				ap.spawnBackgroundChannelsRefresh()
				return nil
			}
		}
	}

	// No usable cache: fetch fresh data synchronously (first run or force)
	ap.channelsMu.Unlock()
	return ap.fetchAndStoreChannels(ctx)
}

// spawnBackgroundChannelsRefresh starts a background goroutine to fetch fresh channel data.
func (ap *ApiProvider) spawnBackgroundChannelsRefresh() {
	if !ap.refreshingChannels.CompareAndSwap(false, true) {
		ap.logger.Debug("Skipping background channels refresh, already in progress")
		return
	}
	go func() {
		defer ap.refreshingChannels.Store(false)
		if err := ap.fetchAndStoreChannels(context.Background()); err != nil {
			ap.logger.Warn("Background channels refresh failed, continuing with stale data",
				zap.Error(err))
		}
	}()
}

// fetchAndStoreChannels fetches all channels from the Slack API and updates the snapshot and cache file.
// Serialized by fetchChannelsMu to prevent concurrent fetches from racing on snapshot/file writes.
func (ap *ApiProvider) fetchAndStoreChannels(ctx context.Context) error {
	ap.fetchChannelsMu.Lock()
	defer ap.fetchChannelsMu.Unlock()

	channels, err := ap.getChannelsMultiType(ctx, AllChanTypes)
	if err != nil {
		// Keep the previous snapshot and cache file: a partial listing must
		// not replace a complete one.
		ap.logger.Error("Failed to fetch channels, keeping existing cache", zap.Error(err))
		return err
	}

	if len(channels) == 0 {
		if ap.channelsReady.Load() {
			ap.logger.Warn("API returned zero channels, keeping existing cache")
			return nil
		}
		return errors.New("API returned zero channels and no existing cache is available")
	}

	newSnapshot := &ChannelsCache{
		Channels:    make(map[string]Channel, len(channels)),
		ChannelsInv: make(map[string]string, len(channels)),
	}
	for _, ch := range channels {
		newSnapshot.Channels[ch.ID] = ch
		newSnapshot.ChannelsInv[ch.Name] = ch.ID
	}
	ap.channelsSnapshot.Store(newSnapshot)

	if data, err := json.MarshalIndent(channels, "", "  "); err != nil {
		ap.logger.Error("Failed to marshal channels for cache", zap.Error(err))
	} else {
		// Atomic write: temp file + rename to prevent partial/corrupt files
		if err := atomicWriteFile(ap.channelsCachePath, data, 0600); err != nil {
			ap.logger.Error("Failed to write cache file",
				zap.String("cache_file", ap.channelsCachePath),
				zap.Error(err))
		} else {
			ap.logger.Info("Wrote channels to cache",
				zap.Int("count", len(channels)),
				zap.String("cache_file", ap.channelsCachePath))
		}
	}

	ap.channelsReady.Store(true)

	return nil
}

// maxRateLimitBackoff caps how long a single Slack Retry-After is honored.
const maxRateLimitBackoff = 60 * time.Second

// channelsPageRetries is how many times a rate-limited channels page is retried.
const channelsPageRetries = 3

// cappedRetryAfter classifies *slack.RateLimitedError as retryable, honoring
// its Retry-After up to maxRateLimitBackoff. Other errors are not retried.
func cappedRetryAfter(err error) time.Duration {
	var rle *slack.RateLimitedError
	if !errors.As(err, &rle) {
		return 0
	}
	switch {
	case rle.RetryAfter <= 0:
		return time.Second
	case rle.RetryAfter > maxRateLimitBackoff:
		return maxRateLimitBackoff
	default:
		return rle.RetryAfter
	}
}

func (ap *ApiProvider) getChannelsMultiType(ctx context.Context, channelTypes []string) ([]Channel, error) {
	chans, err := ap.getChannelsMultiTypeWith(ctx, ap.client, channelTypes)
	if err != nil {
		return nil, err
	}
	if ap.userClient == nil {
		return chans, nil
	}

	userChans, err := ap.getChannelsMultiTypeWith(ctx, ap.userClient, channelTypes)
	if err != nil {
		return nil, fmt.Errorf("user token: %w", err)
	}

	// Merge in channels visible to the user token (e.g. private channels the
	// bot isn't a member of) so names resolve for user-token tools. Entries
	// from the primary client win on ID or name collisions (e.g. @user DMs).
	seenIDs := make(map[string]struct{}, len(chans))
	seenNames := make(map[string]struct{}, len(chans))
	for _, c := range chans {
		seenIDs[c.ID] = struct{}{}
		seenNames[c.Name] = struct{}{}
	}
	for _, c := range userChans {
		if _, ok := seenIDs[c.ID]; ok {
			continue
		}
		if _, ok := seenNames[c.Name]; ok {
			continue
		}
		seenIDs[c.ID] = struct{}{}
		seenNames[c.Name] = struct{}{}
		chans = append(chans, c)
	}
	return chans, nil
}

// getChannelsMultiTypeWith pages through conversations.list. Every page waits
// on the rate limiter and is retried on Slack rate limiting; any other error
// aborts the whole listing so callers never mistake a partial list for a
// complete one.
func (ap *ApiProvider) getChannelsMultiTypeWith(ctx context.Context, client SlackAPI, channelTypes []string) ([]Channel, error) {
	params := &slack.GetConversationsParameters{
		Types:           channelTypes,
		Limit:           999,
		ExcludeArchived: true,
	}

	type page struct {
		channels []slack.Channel
		nextCur  string
	}

	var chans []Channel
	usersMap := ap.ProvideUsersMap().Users
	for {
		pg, err := limiter.CallWithRetry(ctx, ap.rateLimiter, channelsPageRetries, cappedRetryAfter, func() (page, error) {
			channels, nextCur, err := client.GetConversationsContext(ctx, params)
			return page{channels: channels, nextCur: nextCur}, err
		})
		if err != nil {
			return nil, fmt.Errorf("fetching channels: %w", err)
		}
		ap.logger.Debug("Fetched channels",
			zap.Strings("channelTypes", channelTypes),
			zap.Int("count", len(pg.channels)),
		)

		for _, channel := range pg.channels {
			chans = append(chans, MapChannelFromSlack(channel, usersMap))
		}

		if pg.nextCur == "" {
			break
		}

		params.Cursor = pg.nextCur
	}
	return chans, nil
}

func (ap *ApiProvider) ProvideUsersMap() *UsersCache {
	// Atomic load - no lock needed, snapshot is immutable
	return ap.usersSnapshot.Load()
}

func (ap *ApiProvider) ProvideChannelsMaps() *ChannelsCache {
	// Atomic load - no lock needed, snapshot is immutable
	return ap.channelsSnapshot.Load()
}

func (ap *ApiProvider) IsReady() (bool, error) {
	if !ap.usersReady.Load() {
		return false, ErrUsersNotReady
	}
	if !ap.channelsReady.Load() {
		return false, ErrChannelsNotReady
	}
	return true, nil
}

// SkipCache marks both users and channels caches as ready without loading
// any data. Lookups by #channel-name or @username will not work; callers
// must use channel/user IDs instead.
func (ap *ApiProvider) SkipCache() {
	ap.usersReady.Store(true)
	ap.channelsReady.Store(true)
}

func (ap *ApiProvider) ServerTransport() string {
	return ap.transport
}

func (ap *ApiProvider) Slack() SlackAPI {
	return ap.client
}

// UserSlack returns the user-token client for calls that must act as the user
// (channel create/rename/invite, DMs, message deletion). It falls back to the
// primary client when no separate user token is configured.
func (ap *ApiProvider) UserSlack() SlackAPI {
	if ap.userClient != nil {
		return ap.userClient
	}
	return ap.client
}

// SlackAs returns the user-token client when asUser is set and the default
// client otherwise. It fails when asUser is set but only a bot token is configured.
func (ap *ApiProvider) SlackAs(asUser bool) (SlackAPI, error) {
	if !asUser {
		return ap.client, nil
	}
	if ap.UserIsBotToken() {
		return nil, errors.New("as_user requires a user token; set SLACK_MCP_XOXP_TOKEN alongside SLACK_MCP_XOXB_TOKEN")
	}
	return ap.UserSlack(), nil
}

// HasUserClient reports whether a separate user-token client is configured
// alongside the bot token.
func (ap *ApiProvider) HasUserClient() bool {
	return ap.userClient != nil
}

func (ap *ApiProvider) IsBotToken() bool {
	client, ok := ap.client.(*MCPSlackClient)
	return ok && client != nil && client.IsBotToken()
}

// UserIsBotToken reports whether the client returned by UserSlack uses a bot token.
func (ap *ApiProvider) UserIsBotToken() bool {
	client, ok := ap.UserSlack().(*MCPSlackClient)
	return ok && client != nil && client.IsBotToken()
}

// slackUserIDPattern matches Slack user IDs (e.g., U07VCEPP4N5, W0123456789).
var slackUserIDPattern = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)

// SearchUsers searches for users by name, email, or display name.
// If the query matches a Slack user ID pattern (e.g., U07VCEPP4N5), it looks up the user
// directly via the users.info API instead of searching. Otherwise it searches
// the local users cache.
func (ap *ApiProvider) SearchUsers(ctx context.Context, query string, limit int) ([]slack.User, error) {
	if slackUserIDPattern.MatchString(query) {
		users, err := ap.client.GetUsersInfo(query)
		if err != nil {
			return nil, err
		}
		if users != nil {
			return *users, nil
		}
		return nil, nil
	}

	return ap.searchUsersInCache(query, limit)
}

// defaultUserSearchLimit is used when SearchUsers is called with limit <= 0.
const defaultUserSearchLimit = 10

// searchUsersInCache performs a case-insensitive regex search on cached users.
// Matches against username, real name, display name, and email.
func (ap *ApiProvider) searchUsersInCache(query string, limit int) ([]slack.User, error) {
	if !ap.usersReady.Load() {
		return nil, ErrUsersNotReady
	}
	if limit <= 0 {
		limit = defaultUserSearchLimit
	}

	pattern, err := regexp.Compile("(?i)" + regexp.QuoteMeta(query))
	if err != nil {
		return nil, err
	}

	usersCache := ap.usersSnapshot.Load()
	var results []slack.User
	for _, user := range usersCache.Users {
		if user.Deleted {
			continue
		}

		if pattern.MatchString(user.Name) ||
			pattern.MatchString(user.RealName) ||
			pattern.MatchString(user.Profile.DisplayName) ||
			pattern.MatchString(user.Profile.Email) {
			results = append(results, user)

			if len(results) >= limit {
				break
			}
		}
	}

	return results, nil
}

func mapChannel(
	id, name, nameNormalized, topic, purpose, user string,
	members []string,
	numMembers int,
	isIM, isMpIM, isPrivate, isExtShared bool,
	usersMap map[string]slack.User,
) Channel {
	channelName := name
	finalPurpose := purpose
	finalTopic := topic
	finalMemberCount := numMembers

	var userID string
	if isIM {
		finalMemberCount = 2
		userID = user // Store the user ID for later re-mapping

		// If user field is empty but we have members, try to extract from members
		if userID == "" && len(members) > 0 {
			// For IM channels, members should contain the other user's ID
			// Try each member to find a valid user in the users map
			for _, memberID := range members {
				if _, ok := usersMap[memberID]; ok {
					userID = memberID
					break
				}
			}
		}

		if u, ok := usersMap[userID]; ok {
			channelName = "@" + u.Name
			finalPurpose = "DM with " + u.RealName
		} else if userID != "" {
			channelName = "@" + userID
			finalPurpose = "DM with " + userID
		} else {
			channelName = "@"
			finalPurpose = "DM with "
		}
		finalTopic = ""
	} else if isMpIM {
		if len(members) > 0 {
			finalMemberCount = len(members)
			var userNames []string
			for _, uid := range members {
				if u, ok := usersMap[uid]; ok {
					userNames = append(userNames, u.RealName)
				} else {
					userNames = append(userNames, uid)
				}
			}
			channelName = "@" + nameNormalized
			finalPurpose = "Group DM with " + strings.Join(userNames, ", ")
			finalTopic = ""
		}
	} else {
		channelName = "#" + nameNormalized
	}

	return Channel{
		ID:          id,
		Name:        channelName,
		Topic:       finalTopic,
		Purpose:     finalPurpose,
		MemberCount: finalMemberCount,
		IsIM:        isIM,
		IsMpIM:      isMpIM,
		IsPrivate:   isPrivate,
		IsExtShared: isExtShared,
		User:        userID,
		Members:     members,
	}
}

// MapChannelFromSlack converts a slack.Channel to our internal Channel type.
func MapChannelFromSlack(c slack.Channel, usersMap map[string]slack.User) Channel {
	return mapChannel(
		c.ID, c.Name, c.NameNormalized,
		c.Topic.Value, c.Purpose.Value,
		c.User, c.Members, c.NumMembers,
		c.IsIM, c.IsMpIM, c.IsPrivate, c.IsExtShared,
		usersMap,
	)
}
