package agentchat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := LoadState(path)
	require.NoError(t, err)
	assert.Empty(t, s.Subscriptions)

	s.JoinTS["C1"] = "100.000000"
	s.Subscriptions["sess"] = &Subscription{SessionID: "sess", Kind: KindClaude, Socket: "/s", Token: "tok", Channels: []string{"C1"}}
	s.MarkDelivered("sess", "C1", "101.0", time.Unix(5000, 0))
	require.NoError(t, s.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	got, err := LoadState(path)
	require.NoError(t, err)
	assert.Equal(t, "100.000000", got.JoinTS["C1"])
	assert.Equal(t, "tok", got.Subscriptions["sess"].Token)
	assert.True(t, got.WasDelivered("sess", "C1", "101.0"))
	assert.False(t, got.WasDelivered("other", "C1", "101.0"))
}

func TestStatePrune(t *testing.T) {
	s := &State{JoinTS: map[string]string{}, Subscriptions: map[string]*Subscription{}, Delivered: map[string]int64{}}
	s.MarkDelivered("a", "C", "1.0", time.Unix(0, 0))
	s.MarkDelivered("a", "C", "2.0", time.Unix(8*24*3600, 0))
	s.Prune(time.Unix(8*24*3600, 0))
	assert.False(t, s.WasDelivered("a", "C", "1.0"))
	assert.True(t, s.WasDelivered("a", "C", "2.0"))
}

func TestWatchers(t *testing.T) {
	s := &State{Subscriptions: map[string]*Subscription{
		"b": {SessionID: "b", Channels: []string{"C1"}},
		"a": {SessionID: "a", Channels: []string{"C1", "C2"}},
		"c": {SessionID: "c", Channels: []string{"C2"}},
	}}
	ws := s.Watchers("C1")
	require.Len(t, ws, 2)
	assert.Equal(t, "a", ws[0].SessionID)
	assert.Equal(t, "b", ws[1].SessionID)
}

func TestTS(t *testing.T) {
	assert.Equal(t, "1759600000.000123", NowTS(time.Unix(1759600000, 123456)))
	assert.True(t, TSLess("9.000001", "10.000000"))
	assert.True(t, TSLess("10.000001", "10.000002"))
	assert.False(t, TSLess("10.000002", "10.000002"))
}
