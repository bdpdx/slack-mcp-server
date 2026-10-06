package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScriptedPrompter(t *testing.T) {
	p := &Scripted{Answers: []string{"", "custom", "n", "2", "  xoxb-1  "}}
	v, err := p.Ask("Name?", "def")
	require.NoError(t, err)
	assert.Equal(t, "def", v, "empty answer takes the default")
	v, _ = p.Ask("Name?", "def")
	assert.Equal(t, "custom", v)
	ok, _ := p.Confirm("Go?", true)
	assert.False(t, ok)
	i, _ := p.Choose("Pick", []string{"a", "b", "c"}, 0)
	assert.Equal(t, 1, i, "choices are numbered from 1")
	s, _ := p.Secret("Token")
	assert.Equal(t, "xoxb-1", s, "secrets are trimmed")
	_, err = p.Ask("More?", "")
	assert.ErrorIs(t, err, ErrAborted, "running out of answers is EOF")
	assert.Contains(t, p.Out.String(), "Pick")
}

func TestScriptedChooseRejectsOutOfRange(t *testing.T) {
	p := &Scripted{Answers: []string{"9", "x", "3"}}
	i, err := p.Choose("Pick", []string{"a", "b", "c"}, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, i, "invalid answers are asked again")
}
