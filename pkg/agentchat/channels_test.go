package agentchat

import (
	"context"
	"errors"
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
