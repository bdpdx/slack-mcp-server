package setup

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var devNull = io.Discard

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

// Test EOF behavior in all prompter methods
func TestScriptedEOF(t *testing.T) {
	tests := []struct {
		name    string
		method  func(*Scripted) error
		answers []string
	}{
		{
			name: "Secret EOF",
			method: func(p *Scripted) error {
				_, err := p.Secret("token")
				return err
			},
			answers: []string{},
		},
		{
			name: "Confirm EOF",
			method: func(p *Scripted) error {
				_, err := p.Confirm("proceed", false)
				return err
			},
			answers: []string{},
		},
		{
			name: "Choose EOF",
			method: func(p *Scripted) error {
				_, err := p.Choose("pick", []string{"a", "b"}, 0)
				return err
			},
			answers: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Scripted{Answers: tt.answers}
			err := tt.method(p)
			assert.ErrorIs(t, err, ErrAborted)
		})
	}
}

// Test secret trimming with various quote and whitespace patterns
func TestSecretTrimming(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"  xoxb-1  ", "xoxb-1"},
		{`"xoxb-1"`, "xoxb-1"},
		{`'xoxb-1'`, "xoxb-1"},
		{` "xoxb-1"`, "xoxb-1"},
		{`"xoxb-1" `, "xoxb-1"},
		{` "xoxb-1"
`, "xoxb-1"},
		{`  'xoxb-1'  `, "xoxb-1"},
		{"\ttok\t", "tok"},
		{` "token with spaces" `, "token with spaces"},
	}

	for _, tt := range tests {
		t.Run("trim_"+tt.expected, func(t *testing.T) {
			p := &Scripted{Answers: []string{tt.input}}
			s, err := p.Secret("token")
			require.NoError(t, err)
			assert.Equal(t, tt.expected, s)
		})
	}
}

// Test NewTerminal with non-TTY input (pipes, files, etc.)
func TestTerminalNonTTY(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		testFunc  func(*Terminal) (interface{}, error)
		expectErr error
		expectVal interface{}
	}{
		{
			name:  "Ask with CRLF stripped",
			input: "hello\r\n",
			testFunc: func(term *Terminal) (interface{}, error) {
				return term.Ask("q", "")
			},
			expectVal: "hello",
		},
		{
			name:  "Ask final line without newline",
			input: "hello",
			testFunc: func(term *Terminal) (interface{}, error) {
				return term.Ask("q", "")
			},
			expectVal: "hello",
		},
		{
			name:  "Ask empty input returns ErrAborted",
			input: "",
			testFunc: func(term *Terminal) (interface{}, error) {
				_, err := term.Ask("q", "")
				return nil, err
			},
			expectErr: ErrAborted,
		},
		{
			name:  "Secret without TTY reads from stdin",
			input: "my-secret\n",
			testFunc: func(term *Terminal) (interface{}, error) {
				return term.Secret("password")
			},
			expectVal: "my-secret",
		},
		{
			name:  "Confirm with CRLF yes",
			input: "yes\r\n",
			testFunc: func(term *Terminal) (interface{}, error) {
				return term.Confirm("proceed", false)
			},
			expectVal: true,
		},
		{
			name:  "Choose with input",
			input: "2\n",
			testFunc: func(term *Terminal) (interface{}, error) {
				return term.Choose("pick", []string{"a", "b", "c"}, 0)
			},
			expectVal: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			require.NoError(t, err)
			defer r.Close()

			_, err = w.WriteString(tt.input)
			require.NoError(t, err)
			w.Close()

			term := NewTerminal(r, devNull)
			val, err := tt.testFunc(term)

			if tt.expectErr != nil {
				assert.ErrorIs(t, err, tt.expectErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectVal, val)
			}
		})
	}
}

func TestTerminalSecretFromPipe(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	_, err = w.WriteString(" 'xoxb-1' \n")
	require.NoError(t, err)
	w.Close()
	var out strings.Builder
	s, err := NewTerminal(r, &out).Secret("Bot token")
	require.NoError(t, err, "a non-terminal stdin is read as a plain line")
	assert.Equal(t, "xoxb-1", s)
	assert.Contains(t, out.String(), "Bot token (input hidden): ")
}

func TestRestoreOnInterrupt(t *testing.T) {
	restored := false
	code := make(chan int, 1)
	_, err := restoreOnInterrupt(func() { restored = true }, func(c int) { code <- c }, func() ([]byte, error) {
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
		select {
		case c := <-code:
			code <- c
		case <-time.After(5 * time.Second):
		}
		return nil, errors.New("read interrupted")
	})
	assert.Error(t, err)
	assert.True(t, restored, "the terminal state is restored on Ctrl-C")
	assert.Equal(t, 130, <-code)

	restored = false
	b, err := restoreOnInterrupt(func() { restored = true }, func(int) { t.Error("exit called") }, func() ([]byte, error) {
		return []byte("tok"), nil
	})
	require.NoError(t, err)
	assert.Equal(t, "tok", string(b))
	assert.False(t, restored)
}
