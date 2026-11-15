package providerupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// runTimeout bounds one update command: a download plus an install.
	runTimeout = 15 * time.Minute
	// runOutputLimit bounds the output tail a failed run reports.
	runOutputLimit = 8 << 10
)

// Run claims an update of the provider and returns; the run continues on
// its own goroutine. It refuses while that provider's run is running, after
// an update command of the provider outlived its run, when the last check
// found no newer version, and once Stop has run. The run then checks the
// provider again and runs the update that check planned only if it still
// finds a newer version: an updater installs whatever its source holds, so
// an offer that went stale (the source moved to an older version, the
// binary changed) must not reach it.
func (s *Service) Run(providerName string) error {
	providerState, ok := s.providers[providerName]
	if !ok {
		return unknownProvider(providerName)
	}
	var refusal error
	s.update(func() bool {
		if refusal = s.admitLocked(); refusal != nil {
			return false
		}
		status := &providerState.status
		switch {
		case status.Run.Phase == PhaseRunning:
			refusal = ErrRunning
		case providerState.survived:
			refusal = ErrUpdaterSurvived
		case !status.Available:
			refusal = ErrNoUpdate
		}
		if refusal != nil {
			s.work.Done()
			return false
		}
		status.Run = Run{Phase: PhaseRunning, FromVersion: status.InstalledVersion}
		go s.runUpdate(providerName)
		return true
	})
	if refusal != nil {
		return fmt.Errorf("cannot update %s: %w", providerName, refusal)
	}
	return nil
}

// runUpdate checks the provider, runs the update that check planned, lets
// the app notice the new binary, checks again and records the outcome.
func (s *Service) runUpdate(providerName string) {
	defer s.work.Done()
	result := s.checkProvider(providerName)
	run := confirmUpdate(result)
	survived := false
	if run.Phase == PhaseRunning {
		ctx, cancel := context.WithTimeout(s.life, runTimeout)
		out, err := s.deps.Runner(ctx, result.update)
		cancel()
		survived = errors.Is(err, ErrCommandSurvived)
		s.deps.AfterRun()
		// ResolvedPath is not compared: a successful update moves it.
		if now := s.deps.Binary(providerName); err == nil && (now.Path != result.binary.Path || now.Launcher != result.binary.Launcher) {
			run = Run{
				Phase:       PhaseFailed,
				FromVersion: run.FromVersion,
				Error:       fmt.Sprintf("the configured %s binary changed during the update, so its result could not be confirmed", providerName),
				Output:      outputTail(out.Text),
			}
		} else {
			run = s.runOutcome(run.FromVersion, s.deps.Installed(providerName), out, err)
		}
		// The check reads the binary the command left, so latest and
		// available describe it before the outcome frame does. A check that
		// began while the command ran finishes first and cannot be the last
		// word.
		s.checkProvider(providerName)
	}
	s.update(func() bool {
		providerState := s.providers[providerName]
		providerState.status.Run = run
		providerState.survived = providerState.survived || survived
		return true
	})
}

// confirmUpdate judges the check a run just made: a running Run from the
// installed version it read when it still offers an update, otherwise the
// failed Run that says why.
func confirmUpdate(result checked) Run {
	run := Run{Phase: PhaseFailed, FromVersion: result.installed}
	switch {
	case errors.Is(result.err, ErrShuttingDown):
		run.Error = "the update was stopped because the app is shutting down"
	case result.err != nil:
		run.Error = "could not confirm the update before running it: " + result.err.Error()
	case !result.available:
		run.Error = fmt.Sprintf("%s is no longer newer than the installed %s, so nothing was updated",
			result.latest, result.installed)
	default:
		run.Phase = PhaseRunning
	}
	return run
}

// runOutcome judges a finished command by the installed version, not by
// its exit status alone: an updater can exit 0 having changed nothing.
func (s *Service) runOutcome(from, installed string, out Output, err error) Run {
	run := Run{Phase: PhaseFailed, FromVersion: from, ToVersion: installed}
	switch {
	case errors.Is(err, ErrCommandSurvived):
		run.Error = "the update command could not be stopped and may still be changing the installed CLI"
	case err != nil && s.life.Err() != nil:
		run.Error = "the update was stopped because the app is shutting down"
	case errors.Is(err, context.DeadlineExceeded):
		run.Error = fmt.Sprintf("the update command did not finish within %s", runTimeout)
	case err != nil:
		run.Error = fmt.Sprintf("could not run the update command: %v", err)
	case out.ExitCode < 0:
		run.Error = "the update command was ended by a signal"
	case out.ExitCode != 0:
		run.Error = fmt.Sprintf("the update command exited with status %d", out.ExitCode)
	case installed == "":
		run.Error = "the update command finished, but the installed version could not be read"
	default:
		if moved, cmpErr := newer(installed, from); cmpErr != nil {
			run.Error = fmt.Sprintf("the update command finished, but the installed version could not be compared: %v", cmpErr)
		} else if !moved {
			run.Error = fmt.Sprintf("the update command finished, but the installed version is still %s", installed)
		} else {
			return Run{Phase: PhaseSucceeded, FromVersion: from, ToVersion: installed}
		}
	}
	run.Output = outputTail(out.Text)
	return run
}

// outputTail is the end of the command's output, escape sequences
// removed, at most runOutputLimit bytes on a rune boundary.
func outputTail(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(stripANSI(text), "\r\n", "\n"))
	if len(text) <= runOutputLimit {
		return text
	}
	cut := len(text) - runOutputLimit
	for cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut++
	}
	return text[cut:]
}
