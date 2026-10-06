package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeAndPrefix(t *testing.T) {
	assert.Equal(t, "xoxb-1-2", NormalizeToken("  \"xoxb-1-2\"\n"))
	assert.Equal(t, "xoxp-1", NormalizeToken("'xoxp-1'"))
	assert.Empty(t, CheckTokenPrefix("bot", "xoxb-1"))
	assert.Empty(t, CheckTokenPrefix("bot", "xoxe.xoxb-1"), "rotation variant")
	assert.Empty(t, CheckTokenPrefix("user", "xoxp-1"))
	assert.Empty(t, CheckTokenPrefix("app", "xapp-1"))
	assert.Contains(t, CheckTokenPrefix("bot", "xoxp-1"), "xoxb-")
	assert.Contains(t, CheckTokenPrefix("app", ""), "xapp-")
}
