package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeAndPrefix(t *testing.T) {
	// Normalize: outer whitespace and quotes
	assert.Equal(t, "xoxb-1-2", NormalizeToken("  \"xoxb-1-2\"\n"))
	assert.Equal(t, "xoxp-1", NormalizeToken("'xoxp-1'"))
	// Normalize: CRLF
	assert.Equal(t, "xoxb-1", NormalizeToken("xoxb-1\r\n"))
	// Normalize: quotes with inner spaces
	assert.Equal(t, "xoxb-1", NormalizeToken("\" xoxb-1 \""))
	// Normalize: empty string
	assert.Empty(t, NormalizeToken(""))
	// Prefix validation: valid tokens
	assert.Empty(t, CheckTokenPrefix("bot", "xoxb-1"))
	assert.Empty(t, CheckTokenPrefix("bot", "xoxe.xoxb-1"), "rotation variant")
	assert.Empty(t, CheckTokenPrefix("user", "xoxp-1"))
	assert.Empty(t, CheckTokenPrefix("app", "xapp-1"))
	// Prefix validation: invalid prefixes
	assert.Contains(t, CheckTokenPrefix("bot", "xoxp-1"), "xoxb-")
	assert.Contains(t, CheckTokenPrefix("app", ""), "xapp-")
	// Prefix validation: unknown kind
	assert.Contains(t, CheckTokenPrefix("unknown", "xoxb-1"), "unknown token kind")
}
