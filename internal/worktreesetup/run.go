package worktreesetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
	"agent-overflow/internal/procutil"
)

// OutputTailBytes bounds the stdout+stderr tail one setup command keeps. A
// failing install prints its diagnosis last, so the tail is the useful end of
// an arbitrarily long stream.
const OutputTailBytes = 16 * 1024

// The two variables every setup command is given. A recipe has to be able to
// name both checkouts — "symlink .env back to the main checkout" is the
// canonical one — and it can name neither on its own: the worktree path is
// generated per worktree, and the project root is not the command's working
// directory. Without them the only expressible recipe is a copy glob, which
// snapshots and then silently diverges.
//
// The contract is documented for authors in this package's AGENTS.md.
const (
	ProjectRootEnv  = "AO_PROJECT_ROOT"
	WorktreePathEnv = "AO_WORKTREE_PATH"
)

// Run copies the configured files into worktreeRoot and then executes the
// configured commands there, in order, under one shared timeout. It blocks
// until the recipe finishes; the caller owns what a failure means (workflow
// provisioning rolls the worktree back and parks).
func Run(ctx context.Context, projectRoot, worktreeRoot string, config Config) error {
	return RunObserved(ctx, projectRoot, worktreeRoot, config, nil)
}

// RunObserved is Run with progress reported to observer as it happens. A nil
// observer makes it Run exactly — the observer is the ONLY difference between
// the two entry points, so a streaming caller and the workflow's blocking
// caller cannot drift apart in ordering, bounding, or error text.
func RunObserved(ctx context.Context, projectRoot, worktreeRoot string, config Config, observer Observer) error {
	if observer == nil {
		observer = noopObserver{}
	}
	steps := ResolveSteps(config)
	observer.RunStarted(steps)
	err := runSteps(ctx, projectRoot, worktreeRoot, config, steps, observer)
	observer.RunFinished(err)
	return err
}

// runSteps executes the resolved steps in order. Walking the SAME slice the
// observer was handed is what keeps step indices from drifting between what a
// panel renders and what a callback names.
//
// The order — copy, then timeout resolution, then env, then the commands — is
// load-bearing and predates the observer: a recipe whose copy phase fails
// reports the copy failure even when its timeout is also unparseable, and an
// unparseable timeout is refused even by a recipe that runs no commands.
func runSteps(
	ctx context.Context,
	projectRoot, worktreeRoot string,
	config Config,
	steps []Step,
	observer Observer,
) error {
	copyStep, commandSteps := partitionSteps(steps)
	if copyStep != nil {
		observer.StepStarted(copyStep.Index)
		if err := copyEntries(ctx, projectRoot, worktreeRoot, config.Copy); err != nil {
			observer.StepFinished(copyStep.Index, err)
			return err
		}
		observer.StepFinished(copyStep.Index, nil)
	}
	timeout, err := ResolveTimeout(config.Timeout)
	if err != nil {
		return fmt.Errorf("invalid worktree setup timeout %q", config.Timeout)
	}
	commandEnv, err := Env(projectRoot, worktreeRoot)
	if err != nil {
		return err
	}
	// ONE timeout for the whole command sequence — the bound is on the recipe,
	// not per command.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, step := range commandSteps {
		if err := runStepCommand(runCtx, worktreeRoot, commandEnv, timeout, step, observer); err != nil {
			return err
		}
	}
	return nil
}

// partitionSteps splits a resolved step list into its optional copy step and
// its command steps, so the execution order is derived from the step kinds
// rather than from an assumption about slice positions.
func partitionSteps(steps []Step) (*Step, []Step) {
	var copyStep *Step
	commands := make([]Step, 0, len(steps))
	for index := range steps {
		if steps[index].Kind == StepCopy {
			copyStep = &steps[index]
			continue
		}
		commands = append(commands, steps[index])
	}
	return copyStep, commands
}

// runStepCommand executes one command step, reporting its output and outcome
// to the observer and returning the caller-facing failure.
func runStepCommand(
	runCtx context.Context,
	worktreeRoot string,
	commandEnv []string,
	timeout time.Duration,
	step Step,
	observer Observer,
) error {
	observer.StepStarted(step.Index)
	// The run loop owns the command's output sink, so the streaming observer
	// is a SECOND writer here rather than a change to runCommand: the tail is
	// what the failure message quotes, and that must not depend on a
	// subscriber existing.
	tail := procutil.NewTailBuffer(OutputTailBytes)
	err := runCommand(runCtx, worktreeRoot, commandEnv, step.Argv,
		io.MultiWriter(tail, stepWriter{observer: observer, index: step.Index}))
	if err == nil {
		observer.StepFinished(step.Index, nil)
		return nil
	}
	output := strings.TrimSpace(tail.String())
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("worktree setup command %s timed out after %s; output tail: %s", formatArgv(step.Argv), timeout, output)
	} else {
		err = fmt.Errorf("worktree setup command %s failed: %w; output tail: %s", formatArgv(step.Argv), err, output)
	}
	observer.StepFinished(step.Index, err)
	return err
}

// runCommand executes one setup argv in the worktree, streaming its combined
// stdout+stderr into output. It reports only what the process did; the caller
// decides how a failure reads, because only the caller knows whether the run
// context expired.
func runCommand(ctx context.Context, worktreeRoot string, commandEnv, argv []string, output io.Writer) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("worktree setup command has no executable")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = worktreeRoot
	command.Env = commandEnv
	command.Stdout = output
	command.Stderr = output
	procutil.ConfigureGroup(command)
	return command.Run()
}

// Env renders the environment for every setup command: the app's own
// environment so PATH and the user's toolchain survive, plus the two checkout
// paths.
//
// Both are absolutised here rather than trusted from the caller, so the
// contract "these are absolute" holds for whatever a project row or worktree
// record happens to contain. filepath.Abs resolves against the same working
// directory exec resolves a relative command.Dir against, so the variables can
// never name a different tree than the command actually runs in.
//
// They are appended last on purpose: os/exec keeps the final occurrence of a
// duplicated key, so an AO_PROJECT_ROOT the app itself inherited (launching the
// app from inside an agent session is normal) cannot shadow the real one.
func Env(projectRoot, worktreeRoot string) ([]string, error) {
	absoluteProjectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root %q for worktree setup: %w", projectRoot, err)
	}
	absoluteWorktreeRoot, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree path %q for worktree setup: %w", worktreeRoot, err)
	}
	// Setup commands are the user's own toolchain (npm install, bundle,
	// …) — long-lived and PATH-resolving, so the inherited base is
	// scrubbed of AppImage launch artifacts like every other spawned
	// child (see internal/appimage).
	return append(appimage.Scrub(os.Environ()),
		ProjectRootEnv+"="+absoluteProjectRoot,
		WorktreePathEnv+"="+absoluteWorktreeRoot,
	), nil
}

// formatArgv renders an argv for a diagnostic. Same shape the workflow tool
// driver uses, kept local so this package does not depend on the workflow
// packages it is now independent of.
func formatArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for index, argument := range argv {
		quoted[index] = strconv.Quote(argument)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
