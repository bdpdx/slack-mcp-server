package agentchat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectOf(t *testing.T) {
	p, derived := ProjectOf("proj")
	assert.Equal(t, "proj", p)
	assert.False(t, derived)
	p, derived = ProjectOf("my-proj__claude_codex-b")
	assert.Equal(t, "my-proj", p)
	assert.True(t, derived)
	p, derived = ProjectOf("proj__users")
	assert.Equal(t, "proj", p)
	assert.True(t, derived)
}

func TestDerivedChannelNames(t *testing.T) {
	assert.Equal(t, "proj__users", UsersChannelName("proj"))

	name, err := DirectChannelName("proj", "brian", "codex-b")
	require.NoError(t, err)
	assert.Equal(t, "proj__brian_codex-b", name, "the person first, not sorted")

	name, err = SideChannelName("proj", []string{"codex-r", "Claude", "codex-b", "claude"})
	require.NoError(t, err)
	assert.Equal(t, "proj__claude_codex-b_codex-r", name, "sorted, normalized, deduplicated")

	_, err = SideChannelName("proj", []string{"claude", "codex_b"})
	assert.ErrorContains(t, err, "contains _")
	_, err = DirectChannelName("proj", "brian", string(make([]byte, 80)))
	assert.Error(t, err)
}

type fakeMaker struct {
	self     string
	existing map[string]string // name → ID of channels self is in
	taken    map[string]bool   // names taken by channels self is not in
	members  map[string][]string
	created  []string
}

func newFakeMaker(self string) *fakeMaker {
	return &fakeMaker{self: self, existing: map[string]string{}, taken: map[string]bool{}, members: map[string][]string{}}
}

func (f *fakeMaker) CreateConversationContext(_ context.Context, p slack.CreateConversationParams) (*slack.Channel, error) {
	if _, ok := f.existing[p.ChannelName]; ok || f.taken[p.ChannelName] {
		return nil, errors.New("name_taken")
	}
	id := "G" + p.ChannelName
	f.existing[p.ChannelName] = id
	f.members[id] = []string{f.self}
	f.created = append(f.created, p.ChannelName)
	ch := &slack.Channel{}
	ch.ID, ch.Name = id, p.ChannelName
	return ch, nil
}

func (f *fakeMaker) GetConversationsForUserContext(context.Context, *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	var out []slack.Channel
	for name, id := range f.existing {
		ch := slack.Channel{}
		ch.ID, ch.Name = id, name
		out = append(out, ch)
	}
	return out, "", nil
}

func (f *fakeMaker) InviteUsersToConversationContext(_ context.Context, id string, users ...string) (*slack.Channel, error) {
	for _, u := range users {
		if u == f.self {
			return nil, errors.New("cant_invite_self")
		}
		for _, m := range f.members[id] {
			if m == u {
				return nil, errors.New("already_in_channel")
			}
		}
		f.members[id] = append(f.members[id], u)
	}
	return &slack.Channel{}, nil
}

func TestEnsureChannelCreatesThenFinds(t *testing.T) {
	ctx := context.Background()
	f := newFakeMaker("UCL")
	id, created, err := ensureChannel(ctx, f, "proj__brian_claude", "UCL", []string{"UBR", "UCL"})
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, []string{"UCL", "UBR"}, f.members[id], "self is skipped")

	again, created, err := ensureChannel(ctx, f, "proj__brian_claude", "UCL", []string{"UBR", "UCB"})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, id, again)
	assert.Equal(t, []string{"UCL", "UBR", "UCB"}, f.members[id], "existing members do not stop new invites")

	f.taken["proj__claude_codex-b"] = true
	_, _, err = ensureChannel(ctx, f, "proj__claude_codex-b", "UCL", nil)
	assert.ErrorContains(t, err, "exists but this identity is not in it")
}

func TestValidateProjectName(t *testing.T) {
	assert.NoError(t, ValidateProjectName("my-proj"))
	assert.NoError(t, ValidateProjectName(strings.Repeat("a", 32)))
	assert.ErrorContains(t, ValidateProjectName(strings.Repeat("a", 33)), "the limit is 32")
	assert.ErrorContains(t, ValidateProjectName("proj__x"), "cannot contain")
}

// watch start on a project re-adds the side channels this agent's bot is in,
// so a restarted session does not lose them.
func TestDerivedChannels(t *testing.T) {
	f := newFakeMaker("UCB")
	for name, id := range map[string]string{
		"proj": "C1", "proj__brian_codex-b": "C2", "proj__claude_codex-b": "C3", "projector": "C4", "other__proj": "C5",
	} {
		f.existing[name] = id
	}
	ids, err := derivedChannels(context.Background(), f, "proj")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"C2", "C3"}, ids)
	assert.Equal(t, []string{"C1", "C2", "C3"}, uniq([]string{"C1", "C2", "C1", "C3", "C2"}))
}

func TestValidatePlainName(t *testing.T) {
	assert.NoError(t, ValidatePlainName("comms-test"))
	assert.NoError(t, ValidatePlainName(strings.Repeat("a", 80)), "no project-name length limit")
	assert.ErrorContains(t, ValidatePlainName("comms__test"), "derived from a project")
}

func TestMatchUserFindsTheWatcherBot(t *testing.T) {
	person := slack.User{ID: "UBR", Name: "agent-watcher"}
	gone := slack.User{ID: "UOLD", Name: "agent-watcher", IsBot: true, Deleted: true}
	watcher := slack.User{ID: "UAW", Name: "agent_watcher_app", IsBot: true}
	watcher.Profile.DisplayName = "Agent-Watcher"
	u, ok := matchUser([]slack.User{person, gone, watcher}, WatcherBotName, true)
	require.True(t, ok)
	assert.Equal(t, "UAW", u.ID, "a live bot, matched by display name, ignoring case")

	_, ok = matchUser([]slack.User{person, gone}, WatcherBotName, true)
	assert.False(t, ok, "no live bot by that name")
}

type fakeUsers struct {
	users []slack.User
	err   error
}

func (f fakeUsers) GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error) {
	return f.users, f.err
}

func TestAddWatcherInvitesToEveryChannelOnce(t *testing.T) {
	ctx := context.Background()
	f := newFakeMaker("UCL")
	project, _, err := ensureChannel(ctx, f, "proj", "UCL", nil)
	require.NoError(t, err)
	direct, _, err := ensureChannel(ctx, f, "proj__brian_claude", "UCL", []string{"UBR"})
	require.NoError(t, err)
	users := fakeUsers{users: []slack.User{{ID: "UAW", Name: WatcherBotName, IsBot: true}}}
	var w strings.Builder
	addWatcher(ctx, users, f, &w, project, direct)
	addWatcher(ctx, users, f, &w, project, direct)
	assert.Empty(t, w.String(), "a later join finds it already in")
	assert.Equal(t, []string{"UCL", "UAW"}, f.members[project])
	assert.Equal(t, []string{"UCL", "UBR", "UAW"}, f.members[direct])
}

func TestAddWatcherOnlyWarns(t *testing.T) {
	ctx := context.Background()
	f := newFakeMaker("UCL")
	id, _, err := ensureChannel(ctx, f, "proj", "UCL", nil)
	require.NoError(t, err)

	var w strings.Builder
	addWatcher(ctx, fakeUsers{err: errors.New("ratelimited")}, f, &w, id)
	assert.Contains(t, w.String(), "not inviting agent-watcher: ratelimited")

	w.Reset()
	addWatcher(ctx, fakeUsers{users: []slack.User{{ID: "UBR", Name: WatcherBotName}}}, f, &w, id)
	assert.Contains(t, w.String(), "no such bot in this workspace", "a person with that name is not the watcher")

	w.Reset()
	addWatcher(ctx, fakeUsers{users: []slack.User{{ID: "UCL", Name: WatcherBotName, IsBot: true}}}, f, &w, id)
	assert.Contains(t, w.String(), "agent-watcher in Gproj: inviting UCL: cant_invite_self", "a refused invite is reported")
	assert.Equal(t, []string{"UCL"}, f.members[id])
}

type fakeProjectAPI struct {
	*fakeMaker
	fakeUsers
}

// A join of an existing project whose channels lack the watcher puts it back
// in both the project channel and the direct channel, and nowhere else.
func TestProjectJoinRepairsTheWatcherInBothChannels(t *testing.T) {
	ctx := context.Background()
	f := newFakeMaker("UCL")
	project, _, err := ensureChannel(ctx, f, "proj", "UCL", []string{"UBR"})
	require.NoError(t, err)
	direct, _, err := ensureChannel(ctx, f, "proj__brian_claude", "UCL", []string{"UBR"})
	require.NoError(t, err)
	side, _, err := ensureChannel(ctx, f, "proj__claude_codex-b", "UCL", []string{"UBR", "UCB"})
	require.NoError(t, err)
	api := fakeProjectAPI{f, fakeUsers{users: []slack.User{{ID: "UAW", Name: WatcherBotName, IsBot: true}}}}
	me := identity{agentID: "UCL", agentName: "claude", ownerID: "UBR", ownerName: "brian"}
	var w strings.Builder
	ids := projectWatchChannels(ctx, api, &w, me, project, "proj")
	assert.Empty(t, w.String())
	assert.ElementsMatch(t, []string{project, direct, side}, ids)
	assert.Contains(t, f.members[project], "UAW", "the project channel is repaired on join")
	assert.Contains(t, f.members[direct], "UAW", "the direct channel is repaired on join")
	assert.NotContains(t, f.members[side], "UAW", "side channels never get the watcher")
}
