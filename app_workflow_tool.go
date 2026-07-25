package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// workflowToolOutputTailBytes bounds the stdout+stderr tail a tool phase
// attempt keeps. A failing build prints its diagnosis last, so the tail is the
// useful end of an arbitrarily long stream; the whole stream is never buffered.
const workflowToolOutputTailBytes = 32 * 1024

// workflowToolStop records why a tool process stopped being the runner's
// responsibility. The value is set before the process is killed, so the
// goroutine reaping it knows whether to report an outcome or stay silent
// because engine teardown already owns the transition.
type workflowToolStop uint8

const (
	workflowToolActive workflowToolStop = iota
	workflowToolStalled
	workflowToolTornDown
)

// workflowToolAttempt is one live `driver: tool` phase attempt. It holds no
// provider session and no AO thread: the phase is a subprocess, and the run
// record's narrative is where a human reads what it did.
type workflowToolAttempt struct {
	key           engine.RunKey
	phase         def.Phase
	binding       string
	argv          []string
	workspace     string
	narrativePath string
	envelopePath  string
	secrets       profile.ResolvedSecrets
	output        *workflowTailBuffer
	complete      func(engine.Outcome)
	cancel        context.CancelFunc
	watchdog      time.Duration
	started       time.Time

	// lastOutput is nanoseconds since the epoch, written by the output writer
	// on every chunk and read by the watchdog timer.
	lastOutput atomic.Int64

	// timer and stop are owned by the runner mutex.
	timer workflowTimer
	stop  workflowToolStop
}

// startToolPhase runs a phase's profile-bound command. It returns once the
// process is running; the reaping goroutine reports the outcome.
func (r *workflowAppRunner) startToolPhase(ctx context.Context, request engine.RunRequest, complete func(engine.Outcome)) error {
	projectProfile, err := r.projectProfile(ctx, request.Item.ProjectID)
	if err != nil {
		return err
	}
	watchdog, _, err := workflowReliability(projectProfile, request.Phase)
	if err != nil {
		return err
	}
	binding, argv, err := workflowToolCommand(projectProfile, request.Phase, request.Vars)
	if err != nil {
		return errors.Join(engine.ErrWiringFailed, fmt.Errorf("workflow runner: resolve tool phase %q command: %w", request.Phase.ID, err))
	}
	secrets, err := projectProfile.ResolveSecrets()
	if err != nil {
		return errors.Join(engine.ErrSetupFailed, fmt.Errorf("workflow runner: resolve tool phase %q secrets: %w", request.Phase.ID, err))
	}
	prepared, err := r.prepareWorkspace(ctx, request)
	if err != nil {
		return errors.Join(engine.ErrSetupFailed, err)
	}
	narrativePath, envelopePath, err := r.prepareToolAttemptFiles(request.Key)
	if err != nil {
		return errors.Join(engine.ErrSetupFailed, err)
	}
	if err := r.app.store.AttachWorkItemPhaseRun(
		request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt, "", narrativePath,
	); err != nil {
		return fmt.Errorf("workflow runner: attach tool phase run: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}

	// The process outlives Start, so it cannot hang off the engine's startup
	// context — that one is cancelled as soon as Start returns. Application
	// shutdown still tears the process down through the life context.
	processCtx, cancel := context.WithCancel(r.app.lifeCtx())
	attempt := &workflowToolAttempt{
		key: request.Key, phase: request.Phase, binding: binding, argv: argv,
		workspace: prepared.path, narrativePath: narrativePath, envelopePath: envelopePath,
		secrets: secrets, output: newWorkflowTailBuffer(workflowToolOutputTailBytes),
		complete: complete, cancel: cancel, watchdog: watchdog, started: r.now(),
	}
	attempt.lastOutput.Store(attempt.started.UnixNano())

	command := exec.CommandContext(processCtx, argv[0], argv[1:]...)
	command.Dir = prepared.path
	command.Env = append(append(os.Environ(), secrets.Environ()...), "AO_ENVELOPE="+envelopePath)
	writer := &workflowToolWriter{attempt: attempt, now: r.now}
	command.Stdout = writer
	command.Stderr = writer
	configureWorkflowCommand(command)

	runKey := workflowRunKey(request.Key)
	r.mu.Lock()
	if _, exists := r.tools[runKey]; exists {
		r.mu.Unlock()
		cancel()
		return fmt.Errorf("workflow runner: attempt %s is already active", runKey)
	}
	r.tools[runKey] = attempt
	r.mu.Unlock()

	if err := command.Start(); err != nil {
		r.stopToolAttempt(runKey, workflowToolTornDown)
		startErr := fmt.Errorf("workflow runner: start tool phase %q command %s: %w", request.Phase.ID, workflowrunner.FormatArgv(argv), err)
		r.writeToolNarrative(attempt, workflowrunner.ToolReport{
			Outcome:  "the command could not be started: " + err.Error(),
			Envelope: workflowrunner.ToolEnvelopeAbsent,
		})
		return errors.Join(engine.ErrSetupFailed, startErr)
	}
	r.mu.Lock()
	if r.tools[runKey] == attempt {
		attempt.timer = r.newTimer(watchdog, func() { r.toolWatchdogFired(runKey) })
	}
	r.mu.Unlock()

	go r.awaitToolPhase(runKey, attempt, command)

	// A teardown that raced this startup found nothing to stop, so kill the
	// process now rather than leaving it orphaned by its own success.
	if err := ctx.Err(); err != nil {
		r.stopToolAttempt(runKey, workflowToolTornDown)
		return fmt.Errorf("workflow runner: startup cancelled: %w", err)
	}
	return nil
}

// awaitToolPhase reaps the process and is the single place a tool attempt
// reports an outcome. It always writes the attempt narrative, including when
// teardown owns the transition, so a killed command still explains itself.
func (r *workflowAppRunner) awaitToolPhase(runKey string, attempt *workflowToolAttempt, command *exec.Cmd) {
	waitErr := command.Wait()
	attempt.cancel()
	stop := r.detachToolAttempt(runKey, attempt)

	report := workflowrunner.ToolReport{
		PhaseID: attempt.key.PhaseID, Attempt: attempt.key.Attempt,
		Binding: attempt.binding, Argv: attempt.argv, Workspace: attempt.workspace,
		Duration: r.now().Sub(attempt.started), Envelope: workflowrunner.ToolEnvelopeAbsent,
	}
	exitCode, exited := workflowToolExitStatus(waitErr)
	report.Exited, report.ExitCode = exited, exitCode

	switch stop {
	case workflowToolTornDown:
		report.Outcome = "the process was killed by workflow teardown"
		r.writeToolNarrative(attempt, report)
		return
	case workflowToolStalled:
		report.Outcome = fmt.Sprintf("the process was killed after %s without output", attempt.watchdog)
		r.writeToolNarrative(attempt, report)
		attempt.complete(engine.Outcome{Kind: engine.OutcomeStalled})
		return
	}
	if !exited {
		report.Outcome = "the process did not complete: " + waitErr.Error()
		r.writeToolNarrative(attempt, report)
		attempt.complete(engine.Outcome{Kind: engine.OutcomeExecutionFailure})
		return
	}
	report.Outcome = "the command exited"

	payload, source, readErr := r.readToolEnvelope(attempt, exitCode)
	report.Envelope = source
	if readErr != nil {
		report.Outcome = "the command exited but its envelope could not be read: " + readErr.Error()
		r.writeToolNarrative(attempt, report)
		attempt.complete(engine.Outcome{Kind: engine.OutcomeExecutionFailure})
		return
	}
	if validationErr := def.ValidateEnvelope(attempt.phase, payload); validationErr != nil {
		report.Findings = findingsForEnvelopeError(validationErr)
		r.writeToolNarrative(attempt, report)
		// A deterministic command has no feedback turn to correct itself, so
		// this goes straight to the exhaustion outcome an agent reaches after
		// its retry. The partial envelope rides along when it is at least JSON.
		outcome := engine.Outcome{Kind: engine.OutcomeExecutionFailure}
		if json.Valid(payload) {
			outcome.Envelope = payload
		}
		attempt.complete(outcome)
		return
	}
	r.writeToolNarrative(attempt, report)
	outcome, err := workflowrunner.OutcomeFromEnvelope(payload)
	if err != nil {
		log.Printf("workflow runner: tool phase %s: %v", runKey, err)
		attempt.complete(engine.Outcome{Kind: engine.OutcomeExecutionFailure, Envelope: payload})
		return
	}
	attempt.complete(outcome)
}

// readToolEnvelope returns the command's own envelope when it wrote one, and
// otherwise synthesizes one from the exit status. Post-validation is identical
// either way — the caller runs it.
func (r *workflowAppRunner) readToolEnvelope(attempt *workflowToolAttempt, exitCode int) (json.RawMessage, workflowrunner.ToolEnvelopeSource, error) {
	written, present, err := readWorkflowToolEnvelopeFile(attempt.envelopePath)
	if err != nil {
		return nil, workflowrunner.ToolEnvelopeAbsent, err
	}
	if present {
		return workflowrunner.ApplyToolOutputs(written, exitCode), workflowrunner.ToolEnvelopeWritten, nil
	}
	synthesized, err := workflowrunner.SynthesizedToolEnvelope(attempt.phase, exitCode)
	if err != nil {
		return nil, workflowrunner.ToolEnvelopeAbsent, err
	}
	return synthesized, workflowrunner.ToolEnvelopeSynthesized, nil
}

// readWorkflowToolEnvelopeFile reads at most one byte past the envelope size
// cap, so an oversized file fails post-validation with the documented size
// finding instead of being loaded into memory.
func readWorkflowToolEnvelopeFile(path string) (payload []byte, present bool, resultErr error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("envelope path %q is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(def.DefaultEnvelopeSizeCap)+1))
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// prepareToolAttemptFiles creates the attempt directory and clears any envelope
// left by a prior process at the same path, so "the command wrote one" is an
// unambiguous fact rather than an inherited one.
func (r *workflowAppRunner) prepareToolAttemptFiles(key engine.RunKey) (narrativePath, envelopePath string, err error) {
	narrativePath, err = workflowrunner.NarrativePath(r.dataRoot, key.ItemID, key.PhaseID, key.Attempt)
	if err != nil {
		return "", "", err
	}
	envelopePath, err = workflowrunner.EnvelopePath(r.dataRoot, key.ItemID, key.PhaseID, key.Attempt)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(narrativePath), appPrivateDirPerm); err != nil {
		return "", "", fmt.Errorf("workflow runner: create tool phase directory: %w", err)
	}
	if err := os.Remove(envelopePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("workflow runner: clear stale tool envelope: %w", err)
	}
	return narrativePath, envelopePath, nil
}

// writeToolNarrative persists the human-facing record of one tool attempt: the
// command, how it ended, and its masked output tail. Failing to write it is
// visible, never silent, but never changes the phase's outcome.
func (r *workflowAppRunner) writeToolNarrative(attempt *workflowToolAttempt, report workflowrunner.ToolReport) {
	report.PhaseID = attempt.key.PhaseID
	report.Attempt = attempt.key.Attempt
	report.Binding = attempt.binding
	report.Argv = attempt.argv
	report.Workspace = attempt.workspace
	if report.Duration == 0 {
		report.Duration = r.now().Sub(attempt.started)
	}
	report.Output = attempt.secrets.Mask(attempt.output.String())
	report.Truncated = attempt.output.Truncated()
	if err := os.WriteFile(attempt.narrativePath, []byte(workflowrunner.ToolNarrative(report)), appSensitiveFilePerm); err != nil {
		log.Printf("workflow runner: write tool narrative %s: %v", attempt.narrativePath, err)
		r.app.emit("workflow:error", map[string]any{
			"itemId": attempt.key.ItemID,
			"error":  "workflow tool phase narrative could not be written; inspect local diagnostics",
		})
	}
}

// workflowToolExitStatus separates "the command ran and exited" from "the
// process never produced an exit status". A non-zero exit is a result, not a
// failure; a signal kill or a wait error is not a result at all.
func workflowToolExitStatus(waitErr error) (int, bool) {
	if waitErr == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// workflowToolCommand resolves the phase's argv from the live project profile
// and interpolates the phase's variables into every element. Bindings are argv
// arrays in the profile, never shell strings, so nothing here is parsed.
func workflowToolCommand(projectProfile *profile.Profile, phase def.Phase, vars map[string]any) (string, []string, error) {
	var binding string
	var template []string
	var bound bool
	switch {
	case phase.Check != "" && phase.Command != "":
		return "", nil, fmt.Errorf("phase binds both check %q and command %q", phase.Check, phase.Command)
	case phase.Check != "":
		binding = fmt.Sprintf("check %q", phase.Check)
		template, bound = projectProfile.Checks[phase.Check]
	case phase.Command != "":
		binding = fmt.Sprintf("command %q", phase.Command)
		template, bound = projectProfile.Commands[phase.Command]
	default:
		return "", nil, fmt.Errorf("phase declares neither a check nor a command binding")
	}
	if !bound {
		return "", nil, fmt.Errorf("the project profile no longer binds %s", binding)
	}
	if len(template) == 0 || strings.TrimSpace(template[0]) == "" {
		return "", nil, fmt.Errorf("%s binds no executable", binding)
	}
	argv := make([]string, len(template))
	for index, argument := range template {
		interpolated, err := def.Interpolate(argument, phase.Inputs, vars)
		if err != nil {
			return "", nil, fmt.Errorf("%s argument %d: %w", binding, index, err)
		}
		argv[index] = interpolated
	}
	if strings.TrimSpace(argv[0]) == "" {
		return "", nil, fmt.Errorf("%s resolved to an empty executable", binding)
	}
	return binding, argv, nil
}

// workflowToolWriter is the process's combined stdout/stderr sink. Every chunk
// stamps the inactivity clock the watchdog reads.
type workflowToolWriter struct {
	attempt *workflowToolAttempt
	now     func() time.Time
}

func (w *workflowToolWriter) Write(payload []byte) (int, error) {
	w.attempt.lastOutput.Store(w.now().UnixNano())
	return w.attempt.output.Write(payload)
}

// toolWatchdogFired parks a phase whose process has emitted nothing for the
// profile's inactivity window. A late-arriving chunk re-arms the timer for the
// remainder instead of tripping it.
func (r *workflowAppRunner) toolWatchdogFired(runKey string) {
	r.mu.Lock()
	attempt := r.tools[runKey]
	if attempt == nil || attempt.timer == nil {
		r.mu.Unlock()
		return
	}
	idle := r.now().Sub(time.Unix(0, attempt.lastOutput.Load()))
	if idle < attempt.watchdog {
		attempt.timer.Reset(attempt.watchdog - idle)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	r.stopToolAttempt(runKey, workflowToolStalled)
}

// stopToolAttempt detaches an attempt and kills its process group. It is the
// one kill path: teardown, the watchdog, and a cancelled startup all use it.
func (r *workflowAppRunner) stopToolAttempt(runKey string, stop workflowToolStop) (*workflowToolAttempt, bool) {
	r.mu.Lock()
	attempt, ok := r.tools[runKey]
	if ok {
		attempt.stop = stop
		delete(r.tools, runKey)
		r.disarmToolTimerLocked(attempt)
	}
	r.mu.Unlock()
	if ok {
		attempt.cancel()
	}
	return attempt, ok
}

// detachToolAttempt removes a finished attempt and reports why it stopped.
func (r *workflowAppRunner) detachToolAttempt(runKey string, attempt *workflowToolAttempt) workflowToolStop {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.tools[runKey]; ok && current == attempt {
		delete(r.tools, runKey)
		r.disarmToolTimerLocked(attempt)
	}
	return attempt.stop
}

func (r *workflowAppRunner) disarmToolTimerLocked(attempt *workflowToolAttempt) {
	if attempt.timer != nil {
		attempt.timer.Stop()
		attempt.timer = nil
	}
}
