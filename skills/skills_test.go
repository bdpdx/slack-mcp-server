package skills

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFiles(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		files, err := Files(kind)
		require.NoError(t, err, kind)
		assert.Contains(t, files["SKILL.md"], "@BIN@ chat", kind)
		assert.Contains(t, files["SKILL.md"], "COLLABORATION.md", "SKILL.md points to the collaboration guide")
		assert.Contains(t, files["COLLABORATION.md"], "Slack channel: <name>")
		assert.False(t, strings.Contains(files["COLLABORATION.md"], "commit"), "no software-specific guidance")
	}
	_, err := Files("other")
	assert.Error(t, err)
}
