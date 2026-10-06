package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// pagedChannelsClient serves conversations.list from a scripted sequence of
// responses, one per call.
type pagedChannelsClient struct {
	SlackAPI

	mu    sync.Mutex
	steps []channelsStep
	calls int
}

type channelsStep struct {
	channels []slack.Channel
	next     string
	err      error
}

func (m *pagedChannelsClient) GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls >= len(m.steps) {
		return nil, "", errors.New("unexpected extra call")
	}
	s := m.steps[m.calls]
	m.calls++
	return s.channels, s.next, s.err
}

func publicChannel(id, name string) slack.Channel {
	return slack.Channel{GroupConversation: slack.GroupConversation{
		Conversation: slack.Conversation{ID: id, NameNormalized: name},
		Name:         name,
	}}
}

func newChannelsTestProvider(t *testing.T, client SlackAPI) *ApiProvider {
	t.Helper()
	ap := &ApiProvider{
		client:            client,
		logger:            zap.NewNop(),
		rateLimiter:       rate.NewLimiter(rate.Inf, 1),
		channelsCachePath: filepath.Join(t.TempDir(), "channels_cache_v2.json"),
	}
	ap.usersSnapshot.Store(&UsersCache{Users: map[string]slack.User{}, UsersInv: map[string]string{}})
	ap.channelsSnapshot.Store(&ChannelsCache{Channels: map[string]Channel{}, ChannelsInv: map[string]string{}})
	return ap
}

// TestUnitFetchChannelsErrorKeepsSnapshot verifies that a failure mid-pagination
// neither replaces the in-memory snapshot nor writes a partial cache file.
func TestUnitFetchChannelsErrorKeepsSnapshot(t *testing.T) {
	client := &pagedChannelsClient{steps: []channelsStep{
		{channels: []slack.Channel{publicChannel("C1", "general")}, next: "page2"},
		{err: errors.New("internal_error")},
	}}
	ap := newChannelsTestProvider(t, client)

	old := &ChannelsCache{
		Channels:    map[string]Channel{"C0": {ID: "C0", Name: "#old"}, "C9": {ID: "C9", Name: "#other"}},
		ChannelsInv: map[string]string{"#old": "C0", "#other": "C9"},
	}
	ap.channelsSnapshot.Store(old)
	ap.channelsReady.Store(true)
	const oldFile = `[{"id":"C0","name":"#old"},{"id":"C9","name":"#other"}]`
	require.NoError(t, os.WriteFile(ap.channelsCachePath, []byte(oldFile), 0600))

	err := ap.fetchAndStoreChannels(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal_error")

	assert.Same(t, old, ap.channelsSnapshot.Load(), "snapshot must not be replaced")
	data, err := os.ReadFile(ap.channelsCachePath)
	require.NoError(t, err)
	assert.Equal(t, oldFile, string(data), "cache file must not be rewritten")
}

// TestUnitFetchChannelsSuccess verifies a full listing replaces the snapshot
// and is persisted.
func TestUnitFetchChannelsSuccess(t *testing.T) {
	client := &pagedChannelsClient{steps: []channelsStep{
		{channels: []slack.Channel{publicChannel("C1", "general")}, next: "page2"},
		{channels: []slack.Channel{publicChannel("C2", "random")}},
	}}
	ap := newChannelsTestProvider(t, client)

	require.NoError(t, ap.fetchAndStoreChannels(context.Background()))

	snap := ap.channelsSnapshot.Load()
	assert.Len(t, snap.Channels, 2)
	assert.Equal(t, "C2", snap.ChannelsInv["#random"])
	assert.True(t, ap.channelsReady.Load())
	_, err := os.Stat(ap.channelsCachePath)
	assert.NoError(t, err)
}

// TestUnitFetchChannelsRetriesRateLimit verifies rate-limited pages are
// retried after Slack's Retry-After.
func TestUnitFetchChannelsRetriesRateLimit(t *testing.T) {
	client := &pagedChannelsClient{steps: []channelsStep{
		{err: &slack.RateLimitedError{RetryAfter: 10 * time.Millisecond}},
		{channels: []slack.Channel{publicChannel("C1", "general")}},
	}}
	ap := newChannelsTestProvider(t, client)

	chans, err := ap.getChannelsMultiType(context.Background(), AllChanTypes)
	require.NoError(t, err)
	assert.Len(t, chans, 1)
	assert.Equal(t, 2, client.calls)
}

// TestUnitFetchChannelsRateLimitGivesUp verifies retries are bounded.
func TestUnitFetchChannelsRateLimitGivesUp(t *testing.T) {
	steps := make([]channelsStep, channelsPageRetries+1)
	for i := range steps {
		steps[i] = channelsStep{err: &slack.RateLimitedError{RetryAfter: time.Millisecond}}
	}
	client := &pagedChannelsClient{steps: steps}
	ap := newChannelsTestProvider(t, client)

	_, err := ap.getChannelsMultiType(context.Background(), AllChanTypes)
	var rle *slack.RateLimitedError
	assert.True(t, errors.As(err, &rle), "got %v", err)
	assert.Equal(t, channelsPageRetries+1, client.calls)
}

// TestUnitFetchChannelsRateLimitHonorsCancel verifies a long Retry-After
// does not outlive the context.
func TestUnitFetchChannelsRateLimitHonorsCancel(t *testing.T) {
	client := &pagedChannelsClient{steps: []channelsStep{
		{err: &slack.RateLimitedError{RetryAfter: time.Hour}},
	}}
	ap := newChannelsTestProvider(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ap.getChannelsMultiType(ctx, AllChanTypes)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestUnitCappedRetryAfter(t *testing.T) {
	assert.Equal(t, time.Duration(0), cappedRetryAfter(errors.New("boom")))
	assert.Equal(t, time.Second, cappedRetryAfter(&slack.RateLimitedError{}))
	assert.Equal(t, 3*time.Second, cappedRetryAfter(&slack.RateLimitedError{RetryAfter: 3 * time.Second}))
	assert.Equal(t, maxRateLimitBackoff, cappedRetryAfter(&slack.RateLimitedError{RetryAfter: time.Hour}))
	assert.Equal(t, 2*time.Second, cappedRetryAfter(fmt.Errorf("wrapped: %w", &slack.RateLimitedError{RetryAfter: 2 * time.Second})))
}

// gatedUsersClient answers users.info after signalling that it was called.
type gatedUsersClient struct {
	SlackAPI
	called chan struct{}
	user   slack.User
}

func (m *gatedUsersClient) GetUsersInfo(users ...string) (*[]slack.User, error) {
	close(m.called)
	return &[]slack.User{m.user}, nil
}

// TestUnitPatchUserSerializedWithRefresh verifies PatchUser cannot overwrite
// a snapshot stored by a concurrent full refresh.
func TestUnitPatchUserSerializedWithRefresh(t *testing.T) {
	client := &gatedUsersClient{called: make(chan struct{}), user: slack.User{ID: "U3", Name: "carol"}}
	ap := newTestApiProvider(client, &UsersCache{
		Users:    map[string]slack.User{"U1": {ID: "U1", Name: "alice"}},
		UsersInv: map[string]string{"alice": "U1"},
	})

	// Simulate a full refresh in progress.
	ap.fetchUsersMu.Lock()
	var done atomic.Bool
	go func() {
		_, _ = ap.PatchUser(context.Background(), "U3")
		done.Store(true)
	}()
	<-client.called
	time.Sleep(20 * time.Millisecond)
	assert.False(t, done.Load(), "PatchUser must wait for the refresh to finish")

	// The refresh stores its new snapshot, then releases the lock.
	ap.usersSnapshot.Store(&UsersCache{
		Users:    map[string]slack.User{"U1": {ID: "U1", Name: "alice"}, "U2": {ID: "U2", Name: "bob"}},
		UsersInv: map[string]string{"alice": "U1", "bob": "U2"},
	})
	ap.fetchUsersMu.Unlock()

	require.Eventually(t, done.Load, time.Second, 5*time.Millisecond)
	snap := ap.usersSnapshot.Load()
	assert.Contains(t, snap.Users, "U2", "refreshed user must survive the patch")
	assert.Contains(t, snap.Users, "U3", "patched user must be present")
}

func TestUnitSearchUsersInCacheDefaultLimit(t *testing.T) {
	users := map[string]slack.User{}
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("U%03d", i)
		users[id] = slack.User{ID: id, Name: fmt.Sprintf("user%d", i)}
	}
	ap := newTestApiProvider(nil, &UsersCache{Users: users, UsersInv: map[string]string{}})
	ap.usersReady.Store(true)

	for _, limit := range []int{0, -5} {
		res, err := ap.searchUsersInCache("user", limit)
		require.NoError(t, err)
		assert.Len(t, res, defaultUserSearchLimit, "limit %d", limit)
	}

	res, err := ap.searchUsersInCache("user", 3)
	require.NoError(t, err)
	assert.Len(t, res, 3)
}

func TestUnitTokenTypes(t *testing.T) {
	assert.True(t, isBotToken("xoxb-1"))
	assert.True(t, isBotToken("xoxe.xoxb-1"))
	assert.False(t, isBotToken("xoxp-1"))
	assert.True(t, isUserToken("xoxp-1"))
	assert.True(t, isUserToken("xoxe.xoxp-1"))
	for _, tok := range []string{"xoxc-1", "xoxd-1", "demo", ""} {
		assert.False(t, isBotToken(tok) || isUserToken(tok), tok)
		_, err := NewMCPSlackClient(tok, zap.NewNop())
		assert.Error(t, err, tok)
	}
}

func TestUnitTeamAPIURL(t *testing.T) {
	u, err := teamAPIURL("https://acme.slack.com/")
	require.NoError(t, err)
	assert.Equal(t, "https://acme.slack.com/api/", u)

	u, err = teamAPIURL("https://acme.enterprise.slack-gov.com/")
	require.NoError(t, err)
	assert.Equal(t, "https://acme.enterprise.slack-gov.com/api/", u)

	for _, bad := range []string{"", "http://acme.slack.com/", "https://evil.com/", "https://acme.slack.com.evil.com/"} {
		_, err := teamAPIURL(bad)
		assert.Error(t, err, bad)
	}
}
