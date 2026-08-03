package worktreesetup

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultTimeout bounds the whole `Run` sequence when a config names none.
const DefaultTimeout = "10m"

// Config is one project's worktree setup recipe. The zero value is a valid
// "nothing to do".
//
// Run entries are argv arrays, never shell lines: nothing here is parsed, so a
// recipe that wants expansion asks for a shell explicitly
// (`["sh", "-c", "…"]`). Timeout is a time.ParseDuration string; empty means
// DefaultTimeout.
type Config struct {
	Copy    []string   `json:"copy,omitempty"`
	Run     [][]string `json:"run,omitempty"`
	Timeout string     `json:"timeout,omitempty"`
}

// IsZero reports whether the config asks for nothing at all.
func (c Config) IsZero() bool {
	return len(c.Copy) == 0 && len(c.Run) == 0 && strings.TrimSpace(c.Timeout) == ""
}

// Validate returns every independently discoverable problem, joined. Callers
// persist a config only after it validates, so an unrunnable recipe is refused
// at the edit that introduced it rather than at the worktree that needed it.
func Validate(config Config) error {
	var problems []error
	for index, pattern := range config.Copy {
		if strings.TrimSpace(pattern) == "" {
			problems = append(problems, fmt.Errorf("copy[%d]: glob must not be empty", index))
		}
	}
	for index, argv := range config.Run {
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			problems = append(problems, fmt.Errorf("run[%d]: argv must contain a non-empty executable at index 0", index))
		}
	}
	if strings.TrimSpace(config.Timeout) != "" {
		if _, err := ResolveTimeout(config.Timeout); err != nil {
			problems = append(problems, fmt.Errorf("timeout: %w", err))
		}
	}
	return errors.Join(problems...)
}

// ResolveTimeout turns an authored timeout into the duration the run sequence
// is bounded by. It is the one place "" means DefaultTimeout, so validation and
// execution cannot disagree about what an omitted timeout is.
func ResolveTimeout(authored string) (time.Duration, error) {
	value := strings.TrimSpace(authored)
	if value == "" {
		value = DefaultTimeout
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("must be a time.ParseDuration-compatible string")
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("must be greater than 0")
	}
	return timeout, nil
}
