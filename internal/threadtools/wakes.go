package threadtools

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// thread_loop and thread_watch: the calling thread's own schedule
// (docs/specs/standing-agents.md, "Wake supervisor"). Both act on the caller
// alone, so they never forward to another computer, and both are listed by
// thread_status and stopped by thread_cancel by label.

// Wake kinds, as thread_status lists them and as a wake's badge names them.
const (
	KindLoop  = "loop"
	KindWatch = "watch"
)

// LoopCall registers or replaces one loop of the calling thread.
type LoopCall struct {
	Label        string `json:"label"`
	Note         string `json:"note"`
	EverySeconds int    `json:"every_seconds"`
}

// WatchCall registers or replaces one watch of the calling thread. Exactly
// one of EverySeconds and LongRunning is set; TimeoutSeconds bounds one run
// of an interval watch, zero meaning DefaultWakeTimeoutSeconds.
type WatchCall struct {
	Label          string `json:"label"`
	Command        string `json:"command"`
	Cwd            string `json:"cwd,omitempty"`
	EverySeconds   int    `json:"every_seconds,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	LongRunning    bool   `json:"long_running,omitempty"`
}

// WakeState is one loop or watch as thread_status lists it.
type WakeState struct {
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	State        string `json:"state"`
	Note         string `json:"note,omitempty"`
	Command      string `json:"command,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	EverySeconds int64  `json:"every_seconds,omitempty"`
	// TimeoutSeconds bounds one run of an interval watch.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	LongRunning    bool  `json:"long_running,omitempty"`
	// NextFireAt is Unix milliseconds; zero for a long-running watch or a
	// cancelled row.
	NextFireAt  int64  `json:"-"`
	NextFire    string `json:"next_fire,omitempty"`
	LastRunAt   int64  `json:"-"`
	LastRun     string `json:"last_run,omitempty"`
	LastOutcome string `json:"last_outcome,omitempty"`
	// LastOutput is the body of the watch's most recent wake that did not
	// reach the thread, kept until another replaces it or the watch is
	// registered again.
	LastOutput string `json:"last_output,omitempty"`
	// Undelivered is the outcome of the latest wake that did not reach the
	// thread, until a later wake says so; a run that wakes nothing leaves it.
	Undelivered string `json:"undelivered,omitempty"`
	Missed      int64  `json:"missed,omitempty"`
}

// stamped fills the RFC 3339 renderings of the two times.
func (w WakeState) stamped() WakeState {
	w.NextFire, w.LastRun = unixMsToRFC3339(w.NextFireAt), unixMsToRFC3339(w.LastRunAt)
	return w
}

type loopArgs struct {
	Label        string `json:"label"`
	Note         string `json:"note"`
	EverySeconds int    `json:"every_seconds"`
	Stop         bool   `json:"stop"`
}

type watchArgs struct {
	Label          string `json:"label"`
	Command        string `json:"command"`
	EverySeconds   int    `json:"every_seconds"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	LongRunning    bool   `json:"long_running"`
	Cwd            string `json:"cwd"`
	Stop           bool   `json:"stop"`
}

type wakeResult struct {
	WakeState
	Note string `json:"note,omitempty"`
}

func (c *session) loop(ctx context.Context, raw json.RawMessage) (any, error) {
	if Forwarded(ctx) {
		return nil, invalidf("thread_loop schedules the calling thread on its own computer and is not forwarded.")
	}
	var args loopArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	label, err := wakeLabel(args.Label)
	if err != nil {
		return nil, err
	}
	if args.Stop {
		if args.Note != "" || args.EverySeconds != 0 {
			return nil, invalidf("stop takes only the label.")
		}
		return c.stopWake(ctx, label, KindLoop)
	}
	note := trim(args.Note)
	if note == "" {
		return nil, invalidf("note is required: it is what each fire tells you.")
	}
	if utf8.RuneCountInString(note) > MaxNoteRunes {
		return nil, invalidf("note must be at most %d characters.", MaxNoteRunes)
	}
	if err := checkInterval(args.EverySeconds, c.wakeFloor, true); err != nil {
		return nil, err
	}
	state, err := c.app.Loop(ctx, c.caller, LoopCall{Label: label, Note: note, EverySeconds: args.EverySeconds})
	if err != nil {
		return nil, err
	}
	state = state.stamped()
	return wakeResult{WakeState: state, Note: fmt.Sprintf(
		"Loop %q fires next at %s and every %d seconds after, each fire a message in this thread. End your turn: the fire starts a new one. Stop it with thread_cancel label %q or thread_loop stop.",
		label, state.NextFire, args.EverySeconds, label)}, nil
}

func (c *session) watch(ctx context.Context, raw json.RawMessage) (any, error) {
	if Forwarded(ctx) {
		return nil, invalidf("thread_watch schedules the calling thread on its own computer and is not forwarded.")
	}
	var args watchArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	label, err := wakeLabel(args.Label)
	if err != nil {
		return nil, err
	}
	if args.Stop {
		if args.Command != "" || args.EverySeconds != 0 || args.TimeoutSeconds != 0 || args.LongRunning || args.Cwd != "" {
			return nil, invalidf("stop takes only the label.")
		}
		return c.stopWake(ctx, label, KindWatch)
	}
	command := trim(args.Command)
	if command == "" {
		return nil, invalidf("command is required: it is the script this watch runs.")
	}
	if len(command) > MaxWakeCommandBytes {
		return nil, invalidf("command must be at most %d bytes. Put a longer script in a file in your home and run that.", MaxWakeCommandBytes)
	}
	if count := countSet(args.EverySeconds != 0, args.LongRunning); count != 1 {
		return nil, invalidf("Pass exactly one of every_seconds, to run the command on an interval, or long_running, to keep it running. This call passed %d.", count)
	}
	if args.LongRunning {
		if args.TimeoutSeconds != 0 {
			return nil, invalidf("timeout_seconds bounds one run of an interval watch; a long_running watch is restarted when it exits instead.")
		}
	} else {
		if err := checkInterval(args.EverySeconds, c.wakeFloor, false); err != nil {
			return nil, err
		}
		if args.TimeoutSeconds != 0 && (args.TimeoutSeconds < 1 || args.TimeoutSeconds > MaxWakeTimeoutSeconds) {
			return nil, invalidf("timeout_seconds must be between 1 and %d.", MaxWakeTimeoutSeconds)
		}
	}
	state, err := c.app.Watch(ctx, c.caller, WatchCall{
		Label: label, Command: command, Cwd: trim(args.Cwd), EverySeconds: args.EverySeconds,
		TimeoutSeconds: args.TimeoutSeconds, LongRunning: args.LongRunning,
	})
	if err != nil {
		return nil, err
	}
	state = state.stamped()
	note := fmt.Sprintf("Watch %q runs next at %s and every %d seconds after, each run stopped after %d seconds; non-empty stdout wakes this thread.", label, state.NextFire, args.EverySeconds, state.TimeoutSeconds)
	if args.LongRunning {
		note = fmt.Sprintf("Watch %q is running; each stdout line wakes this thread, lines within a second together.", label)
	}
	note += fmt.Sprintf(" A non-zero exit wakes nothing and shows as last_outcome in thread_status. Stop it with thread_cancel label %q or thread_watch stop.", label)
	return wakeResult{WakeState: state, Note: note}, nil
}

func (c *session) stopWake(ctx context.Context, label, kind string) (any, error) {
	state, cancelled, err := c.app.CancelWake(ctx, c.caller, label, kind)
	if err != nil {
		return nil, err
	}
	if !cancelled {
		return cancelResult{Label: label, Kind: kind, State: "none", Effect: EffectNothing,
			Note: fmt.Sprintf("This thread has no live %s named %q; nothing was stopped. thread_status lists what it has.", kind, label)}, nil
	}
	return cancelResult{Label: label, Kind: state.Kind, State: "cancelled", Effect: EffectWakeCancelled,
		Note: cancelNote(EffectWakeCancelled, false)}, nil
}

func wakeLabel(raw string) (string, error) {
	label := trim(raw)
	if label == "" {
		return "", invalidf("label is required: it names the loop or watch in thread_status and thread_cancel.")
	}
	if utf8.RuneCountInString(label) > MaxWakeLabelRunes {
		return "", invalidf("label must be at most %d characters.", MaxWakeLabelRunes)
	}
	return label, nil
}

func checkInterval(seconds, floor int, required bool) error {
	if seconds == 0 && required {
		return invalidf("every_seconds is required: it is how often the loop fires.")
	}
	if seconds < floor || seconds > MaxWakeIntervalSeconds {
		return invalidf("every_seconds must be between %d and %d (30 days): each fire is a turn.", floor, MaxWakeIntervalSeconds)
	}
	return nil
}

func loopSchema(floor int) map[string]any {
	properties := map[string]any{
		"label":         map[string]any{"type": "string", "minLength": 1, "maxLength": MaxWakeLabelRunes},
		"note":          map[string]any{"type": "string", "maxLength": MaxNoteRunes, "description": "Brief wake cue (keep concise; thread keeps its context)."},
		"every_seconds": map[string]any{"type": "integer", "minimum": floor, "maximum": MaxWakeIntervalSeconds},
		"stop":          map[string]any{"type": "boolean"},
	}
	description := "Repeat a wake note every every_seconds until stopped (arrives as data; stop=true with label cancels). Use thread_remind for one-shot."
	return tool("thread_loop", description, properties, "label")
}

func watchSchema(floor int) map[string]any {
	properties := map[string]any{
		"label":           map[string]any{"type": "string", "minLength": 1, "maxLength": MaxWakeLabelRunes},
		"command":         map[string]any{"type": "string", "maxLength": MaxWakeCommandBytes, "description": "Shell command (AO_AGENT_HOME set)."},
		"every_seconds":   map[string]any{"type": "integer", "minimum": floor, "maximum": MaxWakeIntervalSeconds},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": MaxWakeTimeoutSeconds},
		"long_running":    map[string]any{"type": "boolean"},
		"cwd":             map[string]any{"type": "string"},
		"stop":            map[string]any{"type": "boolean"},
	}
	description := "Wake from script stdout (pass every_seconds or long_running; assistants only; stdout arrives as data; errors set last_outcome; stop=true cancels)."
	return tool("thread_watch", description, properties, "label")
}
