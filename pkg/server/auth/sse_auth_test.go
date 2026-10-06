package auth

import "testing"

func TestUnitAPIKey(t *testing.T) {
	env := map[string]string{"SLACK_MCP_SSE_API_KEY": "old"}
	key, deprecated := APIKey(func(k string) string { return env[k] })
	if key != "old" || !deprecated {
		t.Fatalf("APIKey() = %q, %v; want old, true", key, deprecated)
	}
	env["SLACK_MCP_API_KEY"] = "new"
	key, deprecated = APIKey(func(k string) string { return env[k] })
	if key != "new" || deprecated {
		t.Fatalf("APIKey() = %q, %v; want new, false", key, deprecated)
	}
}

func TestUnitCheckAuthorization(t *testing.T) {
	cases := []struct {
		header, key string
		want        bool
	}{
		{"Bearer secret", "secret", true},
		{"secret", "secret", true},
		{"Bearer wrong", "secret", false},
		{"", "secret", false},
		{"", "", false},
		{"Bearer ", "", false},
	}
	for _, c := range cases {
		if got := CheckAuthorization(c.header, c.key); got != c.want {
			t.Errorf("CheckAuthorization(%q, %q) = %v, want %v", c.header, c.key, got, c.want)
		}
	}
}
