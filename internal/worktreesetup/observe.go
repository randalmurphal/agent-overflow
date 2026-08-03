package worktreesetup

import (
	"io"
	"strings"
)

// StepKind separates the one copy phase from the authored commands. The panel
// renders them the same way; the kind exists so a caller can tell which failure
// it is looking at without parsing a label.
type StepKind string

const (
	StepCopy    StepKind = "copy"
	StepCommand StepKind = "command"
)

// CopyStepLabel names the copy phase in a resolved step list. It is a step
// rather than a preamble so its failure surfaces exactly like a command's.
const CopyStepLabel = "Copy files"

// Step is one resolved unit of a run, in execution order. Index is the step's
// position in the slice ResolveSteps returned, and is what every observer
// callback refers to.
type Step struct {
	Index int
	Kind  StepKind
	Label string
	// Argv is the executed command for StepCommand steps, nil for the copy
	// step. It is the exact argv — nothing here is shell-parsed.
	Argv []string
}

// Observer receives a run's progress. Every callback is invoked from the
// goroutine driving the run, in order, and must not block: the run is the
// caller. Output's chunk is only valid for the duration of the call — an
// implementation that retains it must copy.
//
// The blocking Run is this same engine with a no-op observer, so there is one
// execution path and a streaming caller cannot diverge from workflow behavior.
type Observer interface {
	RunStarted(steps []Step)
	StepStarted(index int)
	Output(index int, chunk []byte)
	StepFinished(index int, err error)
	RunFinished(err error)
}

// noopObserver is what Run supplies. Its existence is why there is no
// observer-less code path to keep in sync.
type noopObserver struct{}

func (noopObserver) RunStarted([]Step)       {}
func (noopObserver) StepStarted(int)         {}
func (noopObserver) Output(int, []byte)      {}
func (noopObserver) StepFinished(int, error) {}
func (noopObserver) RunFinished(error)       {}

// ResolveSteps renders the steps a config will execute, in order. The copy
// phase is present only when the recipe names globs — a step that provably
// does nothing is noise in a progress list, and the indices stay contiguous
// either way because callers address steps by position in this slice.
func ResolveSteps(config Config) []Step {
	steps := make([]Step, 0, len(config.Run)+1)
	if len(config.Copy) > 0 {
		steps = append(steps, Step{Index: len(steps), Kind: StepCopy, Label: CopyStepLabel})
	}
	for _, argv := range config.Run {
		steps = append(steps, Step{
			Index: len(steps),
			Kind:  StepCommand,
			Label: commandLabel(argv),
			Argv:  argv,
		})
	}
	return steps
}

// commandLabel renders an argv for a human reading a progress list. The
// diagnostic form (quoted, bracketed) stays in formatArgv — it belongs in an
// error message, not in a panel row.
func commandLabel(argv []string) string {
	return strings.Join(argv, " ")
}

// stepWriter forwards a command's combined output to the observer, tagged with
// the step it came from. It is the SECOND writer at the run loop's output seam;
// the tail buffer stays the first, because the failure message quotes the tail
// and that must not depend on a subscriber existing.
type stepWriter struct {
	observer Observer
	index    int
}

func (w stepWriter) Write(payload []byte) (int, error) {
	w.observer.Output(w.index, payload)
	return len(payload), nil
}

var _ io.Writer = stepWriter{}
