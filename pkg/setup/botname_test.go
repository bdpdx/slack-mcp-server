package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateBotName(t *testing.T) {
	for _, ok := range []string{"claude", "codex-b", "mike-claude2"} {
		assert.Empty(t, ValidateBotName(ok), ok)
	}
	assert.Contains(t, ValidateBotName("codex_b"), "separators in channel names")
	assert.Contains(t, ValidateBotName("codex.b"), "separators in channel names")
	assert.Contains(t, ValidateBotName("users"), "reserved")
	assert.Contains(t, ValidateBotName("Claude"), "lowercase")
	assert.Contains(t, ValidateBotName(""), "empty")
	assert.Contains(t, ValidateBotName("a b"), "lowercase")
}
