package setup

import (
	"encoding/json"
	"errors"
	"os"
)

// State remembers answers between runs to offer them as defaults. It never
// holds tokens.
type State struct {
	Bin   string      `json:"bin"`
	Homes []HomeState `json:"homes"`
}

// HomeState is one configured agent home.
type HomeState struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Bot  string `json:"bot,omitempty"`
}

// LoadState reads path; a missing file is an empty state.
func LoadState(path string) (*State, error) {
	st := &State{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	return st, json.Unmarshal(data, st)
}

// Save writes the state, private to the user.
func (s *State) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'), 0o600)
}

// Home returns the saved home at path, or nil.
func (s *State) Home(path string) *HomeState {
	for i := range s.Homes {
		if s.Homes[i].Path == path {
			return &s.Homes[i]
		}
	}
	return nil
}

// SetHome adds or replaces the home with h.Path.
func (s *State) SetHome(h HomeState) {
	if old := s.Home(h.Path); old != nil {
		*old = h
		return
	}
	s.Homes = append(s.Homes, h)
}
