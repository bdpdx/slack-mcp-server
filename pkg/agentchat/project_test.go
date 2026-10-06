package agentchat

import (
	"context"
	"errors"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeArchiver struct {
	chans    []slack.Channel
	creator  map[string]string
	archived []string
	fail     map[string]bool
}

func (f *fakeArchiver) GetConversationsForUserContext(context.Context, *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	return f.chans, "", nil
}

func (f *fakeArchiver) GetConversationInfoContext(_ context.Context, in *slack.GetConversationInfoInput) (*slack.Channel, error) {
	ch := &slack.Channel{}
	ch.ID, ch.Creator = in.ChannelID, f.creator[in.ChannelID]
	return ch, nil
}

func (f *fakeArchiver) ArchiveConversationContext(_ context.Context, id string) error {
	if f.fail[id] {
		return errors.New("restricted_action")
	}
	f.archived = append(f.archived, id)
	return nil
}

func channel(id, name string) slack.Channel {
	ch := slack.Channel{}
	ch.ID, ch.Name = id, name
	return ch
}

func newProjectFake(creator string) *fakeArchiver {
	return &fakeArchiver{
		chans: []slack.Channel{
			channel("C1", "proj"), channel("C2", "proj__users"), channel("C3", "proj__brian_claude"),
			channel("C4", "proj__claude_codex-b"), channel("C5", "projector"), channel("C6", "other__proj"),
		},
		creator: map[string]string{"C1": creator},
	}
}

func TestProjectChannelsSelectsTheProjectLast(t *testing.T) {
	chans, err := projectChannels(context.Background(), newProjectFake("UBR"), "proj", "UBR", "UCL")
	require.NoError(t, err)
	var names []string
	for _, ch := range chans {
		names = append(names, ch.Name)
	}
	assert.Equal(t, []string{"proj__users", "proj__brian_claude", "proj__claude_codex-b", "proj"}, names,
		"derived channels first, the project last; #projector and #other__proj are not part of it")
}

// Only the user Slack records as the project channel's creator may archive
// it (or, for projects created before that, this home's bot).
func TestProjectChannelsRequiresTheCreator(t *testing.T) {
	ctx := context.Background()
	_, err := projectChannels(ctx, newProjectFake("UMI"), "proj", "UBR", "UCL")
	assert.ErrorIs(t, err, errNotProjectOwner)
	_, err = projectChannels(ctx, newProjectFake("UCB"), "proj", "UBR", "UCL")
	assert.ErrorIs(t, err, errNotProjectOwner, "another agent's bot")
	_, err = projectChannels(ctx, newProjectFake(""), "proj", "UBR", "UCL")
	assert.ErrorIs(t, err, errNotProjectOwner, "unknown creator")

	_, err = projectChannels(ctx, newProjectFake("UCL"), "proj", "UBR", "UCL")
	assert.NoError(t, err, "a legacy project this home's bot created")

	_, err = projectChannels(ctx, newProjectFake("UBR"), "nope", "UBR", "UCL")
	assert.ErrorContains(t, err, "not in an unarchived channel named #nope")
}

func TestArchiveChannelsCarriesOnPastFailures(t *testing.T) {
	f := newProjectFake("UBR")
	f.fail = map[string]bool{"C3": true}
	done, err := archiveChannels(context.Background(), f, []projectChannel{{"C2", "proj__users"}, {"C3", "proj__brian_claude"}, {"C1", "proj"}})
	assert.ErrorContains(t, err, "#proj__brian_claude: restricted_action")
	assert.Equal(t, []string{"C2", "C1"}, f.archived)
	assert.Len(t, done, 2)
}

func TestListenerDropsArchivedChannels(t *testing.T) {
	l := newTestListener(t, newFakeSlack(), &fakeDeliverer{})
	sub := claudeSub("s1")
	sub.Channels = []string{"C1", "C2"}
	require.NoError(t, l.Subscribe(context.Background(), sub, 0))
	l.HandleMessage(context.Background(), Message{Channel: "C2", TS: "2001.1", User: "UBR", SubType: "group_archive"})
	require.Len(t, l.Status(), 1)
	assert.Equal(t, []string{"C1"}, l.Status()[0].Channels)
}
