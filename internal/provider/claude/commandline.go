package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/provider"
)

// Retention is a Claude launch's answer to the transcript sweep: the config
// home the process runs against and what it carries there (retention.go).
// The zero value is no answer, and NewCommandLine refuses it, so a launch
// cannot be built without one.
type Retention struct {
	home retentionHome
	// days is the `cleanupPeriodDays` the process carries, 0 for none.
	days int
	// configDir is the isolated config home.
	configDir string
}

type retentionHome int

const (
	retentionUnanswered retentionHome = iota
	// retentionUserHome may run the sweep over the user's home.
	retentionUserHome
	// retentionUserHomeNoSweep runs against the user's home and is proven
	// never to start the sweep.
	retentionUserHomeNoSweep
	// retentionIsolatedHome runs in a config home that holds no
	// transcripts.
	retentionIsolatedHome
)

// UserHomeRetention answers for a launch against the user's home that can
// run the sweep. days is a CleanupPeriodOverride result for the launch's
// working directory: the process carries it, or nothing when it is 0.
func UserHomeRetention(days int) Retention {
	return Retention{home: retentionUserHome, days: max(days, 0)}
}

// NoSweepRetention answers for a command against the user's home that is
// proven never to start the sweep (`claude mcp list`). It carries nothing.
func NoSweepRetention() Retention {
	return Retention{home: retentionUserHomeNoSweep}
}

// IsolatedHomeRetention answers for a launch whose config home is configDir,
// a directory that holds no transcripts (a sign-in). It carries nothing.
func IsolatedHomeRetention(configDir string) Retention {
	return Retention{home: retentionIsolatedHome, configDir: configDir}
}

// CleanupPeriodDays is the `cleanupPeriodDays` the launch carries, 0 for
// none.
func (r Retention) CleanupPeriodDays() int { return r.days }

// errNoRetention refuses a launch built without a Retention.
var errNoRetention = errors.New("claude: the launch gives no transcript retention answer")

// CommandLine is one Claude CLI launch: the arguments after the binary and
// the environment rules the child runs with. NewCommandLine is the only way
// to build one, and every process the app starts from the Claude binary
// takes its arguments and environment from one.
type CommandLine struct {
	args   []string
	pins   map[provider.ReservedEnv]string
	clears []provider.ReservedEnv
}

// NewCommandLine builds a launch's command line from its Retention, its own
// `--settings` object (a JSON object, "" for none) and its arguments. The
// settings block comes first, ahead of any subcommand, and holds the
// object's keys followed by the retention's `cleanupPeriodDays`; settings
// must not set that key itself. No block is sent when both are empty.
func NewCommandLine(retention Retention, settings string, args ...string) (CommandLine, error) {
	if retention.home == retentionUnanswered {
		return CommandLine{}, errNoRetention
	}
	if retention.home == retentionIsolatedHome && strings.TrimSpace(retention.configDir) == "" {
		return CommandLine{}, errors.New("claude: an isolated launch needs its config directory")
	}
	block, err := settingsWithCleanupPeriod(settings, retention.days)
	if err != nil {
		return CommandLine{}, err
	}
	line := CommandLine{clears: launchClears()}
	if block != "" {
		line.args = append(line.args, "--settings", block)
	}
	line.args = append(line.args, args...)
	if retention.home == retentionIsolatedHome {
		line.pins = map[provider.ReservedEnv]string{provider.ClaudeConfigDirEnv: retention.configDir}
	}
	return line, nil
}

// Args returns the arguments after the binary.
func (c CommandLine) Args() []string { return append([]string(nil), c.args...) }

// Env returns in with the variables the launch clears and pins added.
func (c CommandLine) Env(in provider.EnvInput) provider.EnvInput {
	in = in.Clear(c.clears...)
	for env, value := range c.pins {
		in = in.Pin(env, value)
	}
	return in
}

// launchClears is what every Claude child is cleared of: the config-home
// variables (pinned instead for an isolated home), the cross-session
// variables and the task-list key. A launch that owns one of them pins it,
// which applies after the removal.
func launchClears() []provider.ReservedEnv {
	clears := configHomeClears()
	clears = append(clears, crossSessionClears()...)
	return append(clears, provider.ClaudeTaskListIDEnv)
}

// settingsWithCleanupPeriod appends `"cleanupPeriodDays":days` to the JSON
// object settings, keeping the object's own key order. It returns "" when
// settings is empty and days is 0.
func settingsWithCleanupPeriod(settings string, days int) (string, error) {
	settings = strings.TrimSpace(settings)
	if settings != "" {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(settings), &keys); err != nil || keys == nil {
			return "", fmt.Errorf("claude: the --settings block is not a JSON object: %q", clipSettingValue(json.RawMessage(settings)))
		}
		if _, set := keys[CleanupPeriodDaysSetting]; set {
			return "", fmt.Errorf("claude: the --settings block sets %s, which only the launch's Retention may set", CleanupPeriodDaysSetting)
		}
		if len(keys) == 0 {
			settings = ""
		}
	}
	if days <= 0 {
		return settings, nil
	}
	entry := strconv.Quote(CleanupPeriodDaysSetting) + ":" + strconv.Itoa(days)
	if settings == "" {
		return "{" + entry + "}", nil
	}
	return strings.TrimSuffix(settings, "}") + "," + entry + "}", nil
}
