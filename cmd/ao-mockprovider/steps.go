package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
)

// runSteps executes a step list in order. reportSteps is true for
// top-level onStart/turn steps (harness tests await those boundaries);
// approval branch steps run unreported — approval_decided already marks
// the boundary.
func (e *engine) runSteps(vars scenario.Vars, turn int, steps []scenario.Step, reportSteps bool) bool {
	for i, step := range steps {
		if e.turnAborted(turn) {
			return true
		}
		if reportSteps {
			e.rep.report(control.Report{Kind: control.ReportStepStarted, Turn: turn, Step: i + 1, Detail: stepName(step)})
		}
		e.runStep(vars, turn, step)
		if e.turnAborted(turn) {
			return true
		}
		if reportSteps {
			e.rep.report(control.Report{Kind: control.ReportStepCompleted, Turn: turn, Step: i + 1, Detail: stepName(step)})
		}
	}
	return false
}

func (e *engine) runStep(vars scenario.Vars, turn int, step scenario.Step) {
	switch {
	case step.Emit != nil:
		e.runEmit(vars, step.Emit)
	case step.Fixture != nil:
		e.runFixture(vars, step.Fixture)
	case step.DelayMs != 0:
		sleepMs(step.DelayMs)
	case step.WriteFile != nil:
		e.runWriteFile(step.WriteFile)
	case step.Approval != nil:
		e.runApproval(vars, turn, step.Approval)
	case step.WaitSignal != nil:
		e.runWaitSignal(turn, step.WaitSignal)
	case step.Stall != nil:
		e.runStall(turn, step.Stall)
	case step.Exit != nil:
		e.runExit(step.Exit)
	default:
		// Parse-time Validate guarantees exactly one action; an empty
		// step here means the schema grew without this switch.
		log.Printf("BUG: step with no recognized action (turn %d)", turn)
	}
}

func stepName(step scenario.Step) string {
	switch {
	case step.Emit != nil:
		return "emit"
	case step.Fixture != nil:
		return "fixture"
	case step.DelayMs != 0:
		return "delay"
	case step.WriteFile != nil:
		return "writeFile"
	case step.Approval != nil:
		return "approval"
	case step.WaitSignal != nil:
		return "waitSignal"
	case step.Stall != nil:
		return "stall"
	case step.Exit != nil:
		return "exit"
	}
	return "unknown"
}

func (e *engine) runEmit(vars scenario.Vars, em *scenario.EmitStep) {
	for i, line := range em.Lines {
		if i > 0 && em.DelayBetweenMs > 0 {
			sleepMs(em.DelayBetweenMs)
		}
		e.w.writeLine(vars.Substitute(line), em.ChunkBytes, em.ChunkIntervalMs)
	}
}

// runFixture streams a captured wire log. FromLine/ToLine are 1-indexed
// inclusive against the ORIGINAL file line numbers (before blank/#
// filtering) so ranges quoted in fixture READMEs stay valid.
func (e *engine) runFixture(vars scenario.Vars, fx *scenario.FixtureStep) {
	path := fx.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.fixtureRoot, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("fixture step failed: %v (step skipped)", err)
		return
	}
	emitted := false
	for idx, raw := range strings.Split(string(data), "\n") {
		lineNo := idx + 1
		if fx.FromLine > 0 && lineNo < fx.FromLine {
			continue
		}
		if fx.ToLine > 0 && lineNo > fx.ToLine {
			break
		}
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if emitted && fx.DelayBetweenMs > 0 {
			sleepMs(fx.DelayBetweenMs)
		}
		emitted = true
		e.w.writeLine(vars.Substitute(line), 0, 0)
	}
}

// runWriteFile mutates the workspace (the mock's cwd). Escaping paths
// fail the step loudly but keep the scenario running — a bad path in a
// test script must not take the whole mock down mid-scenario.
func (e *engine) runWriteFile(wf *scenario.WriteFileStep) {
	rel, err := normalizeWorkspaceRel(wf.Path)
	if err != nil {
		log.Printf("writeFile step rejected: %v (step skipped)", err)
		return
	}
	full := filepath.Join(e.cwd, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		log.Printf("writeFile step failed: %v (step skipped)", err)
		return
	}
	flags := os.O_CREATE | os.O_WRONLY
	if wf.Append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(full, flags, 0o644)
	if err != nil {
		log.Printf("writeFile step failed: %v (step skipped)", err)
		return
	}
	_, werr := f.WriteString(wf.Content)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		log.Printf("writeFile step failed: write=%v close=%v", werr, cerr)
	}
}

// normalizeWorkspaceRel validates a workspace-relative path: non-empty,
// not absolute, and not escaping the workspace via "..".
func normalizeWorkspaceRel(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("absolute path %q not allowed", p)
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	return clean, nil
}

func (e *engine) runApproval(vars scenario.Vars, turn int, ap *scenario.ApprovalStep) {
	e.rep.report(control.Report{Kind: control.ReportApprovalPending, Turn: turn, Detail: ap.ToolName})

	decisionCh, cancel, err := e.adapter.sendApproval(ap, vars)
	if err != nil {
		log.Printf("approval step: sending %s request failed: %v (running onDeny)", ap.ToolName, err)
		e.rep.report(control.Report{Kind: control.ReportApprovalDecided, Turn: turn, Detail: "deny"})
		e.runSteps(vars, turn, ap.OnDeny, false)
		return
	}

	allow := false
	detail := "deny"
	if ap.TimeoutMs > 0 {
		timer := time.NewTimer(time.Duration(ap.TimeoutMs) * time.Millisecond)
		select {
		case allow = <-decisionCh:
			timer.Stop()
		case <-e.turnAbortSignal(turn):
			timer.Stop()
			cancel()
			return
		case <-timer.C:
			cancel()
			detail = "timeout"
			log.Printf("approval step: no %s decision within %dms (running onDeny)", ap.ToolName, ap.TimeoutMs)
		}
	} else {
		select {
		case allow = <-decisionCh:
		case <-e.turnAbortSignal(turn):
			cancel()
			return
		}
	}
	if allow {
		detail = "allow"
	}
	e.rep.report(control.Report{Kind: control.ReportApprovalDecided, Turn: turn, Detail: detail})

	branch := ap.OnDeny
	if allow {
		branch = ap.OnAllow
	}
	e.runSteps(vars, turn, branch, false)
}

func (e *engine) runWaitSignal(turn int, ws *scenario.WaitSignalStep) {
	e.rep.report(control.Report{Kind: control.ReportWaitingSignal, Turn: turn, Detail: ws.Name})
	if ch := e.openGate(turn, ws.Name); ch != nil {
		<-ch
	}
}

func (e *engine) runStall(turn int, st *scenario.StallStep) {
	if st.DurationMs > 0 {
		sleepMs(st.DurationMs)
		return
	}
	// DurationMs 0: stall until a control advance (or process close).
	e.rep.report(control.Report{Kind: control.ReportWaitingSignal, Turn: turn, Detail: "stall"})
	if ch := e.openGate(turn, ""); ch != nil {
		<-ch
	}
}

func (e *engine) runExit(ex *scenario.ExitStep) {
	// Stdout writes are unbuffered syscalls, so there is nothing to
	// flush; FlushDelayMs still gives the app's reader time to drain.
	if ex.FlushDelayMs > 0 {
		sleepMs(ex.FlushDelayMs)
	}
	e.terminate(ex.Code)
}

func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}
