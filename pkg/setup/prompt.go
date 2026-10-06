// Package setup is the interactive installer behind `slack-mcp-server
// setup`: it configures Claude Code and Codex homes for Slack agent chat.
package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrAborted ends setup when input runs out (EOF, Ctrl-D).
var ErrAborted = errors.New("setup aborted")

// Prompter asks the user questions. Ask returns def for an empty answer;
// Choose numbers options from 1 and returns the chosen index; Secret reads
// without echo and trims whitespace and wrapping quotes.
type Prompter interface {
	Say(format string, a ...any)
	Ask(question, def string) (string, error)
	Confirm(question string, def bool) (bool, error)
	Choose(question string, options []string, def int) (int, error)
	Secret(question string) (string, error)
}

// lineSource returns one answer line or ErrAborted.
type lineSource func(secret bool) (string, error)

type prompter struct {
	out  io.Writer
	next lineSource
}

func (p *prompter) Say(format string, a ...any) { fmt.Fprintf(p.out, format+"\n", a...) }

func (p *prompter) Ask(q, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", q)
	}
	s, err := p.next(false)
	if err != nil {
		return "", err
	}
	if s = strings.TrimSpace(s); s == "" {
		return def, nil
	}
	return s, nil
}

func (p *prompter) Confirm(q string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		s, err := p.Ask(q+" ("+hint+")", "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(s) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.Say("Please answer y or n.")
	}
}

func (p *prompter) Choose(q string, options []string, def int) (int, error) {
	p.Say("%s", q)
	for i, o := range options {
		p.Say("  %d) %s", i+1, o)
	}
	for {
		s, err := p.Ask("Choice", strconv.Itoa(def+1))
		if err != nil {
			return 0, err
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		p.Say("Please enter a number from 1 to %d.", len(options))
	}
}

func (p *prompter) Secret(q string) (string, error) {
	fmt.Fprintf(p.out, "%s (input hidden): ", q)
	s, err := p.next(true)
	fmt.Fprintln(p.out)
	if err != nil {
		return "", err
	}
	return strings.Trim(strings.TrimSpace(s), `"'`), nil
}

// Terminal prompts on a real terminal, hiding secrets.
type Terminal struct{ prompter }

// NewTerminal reads answers from in and writes prompts to out.
func NewTerminal(in *os.File, out io.Writer) *Terminal {
	r := bufio.NewReader(in)
	t := &Terminal{prompter{out: out}}
	t.next = func(secret bool) (string, error) {
		if fd := int(in.Fd()); secret && term.IsTerminal(fd) {
			state, err := term.GetState(fd)
			if err != nil {
				return "", ErrAborted
			}
			restore := func() {
				_ = term.Restore(fd, state)
				fmt.Fprintln(out)
			}
			b, err := restoreOnInterrupt(restore, os.Exit, func() ([]byte, error) { return term.ReadPassword(fd) })
			if err != nil {
				return "", ErrAborted
			}
			return string(b), nil
		}
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return "", ErrAborted
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	return t
}

// restoreOnInterrupt runs read; if SIGINT (Ctrl-C) arrives meanwhile it
// calls restore (turning terminal echo back on) and exit(130).
func restoreOnInterrupt(restore func(), exit func(int), read func() ([]byte, error)) ([]byte, error) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-sigs:
			restore()
			exit(130)
		case <-done:
		}
	}()
	b, err := read()
	signal.Stop(sigs)
	close(done)
	<-finished
	return b, err
}

// Scripted answers prompts from a list, for tests; Out records the output.
type Scripted struct {
	Answers []string
	Out     strings.Builder
	p       *prompter
}

func (s *Scripted) get() *prompter {
	if s.p == nil {
		s.p = &prompter{out: &s.Out, next: func(bool) (string, error) {
			if len(s.Answers) == 0 {
				return "", ErrAborted
			}
			a := s.Answers[0]
			s.Answers = s.Answers[1:]
			return a, nil
		}}
	}
	return s.p
}

func (s *Scripted) Say(f string, a ...any)                 { s.get().Say(f, a...) }
func (s *Scripted) Ask(q, d string) (string, error)        { return s.get().Ask(q, d) }
func (s *Scripted) Confirm(q string, d bool) (bool, error) { return s.get().Confirm(q, d) }
func (s *Scripted) Choose(q string, o []string, d int) (int, error) {
	return s.get().Choose(q, o, d)
}
func (s *Scripted) Secret(q string) (string, error) { return s.get().Secret(q) }
