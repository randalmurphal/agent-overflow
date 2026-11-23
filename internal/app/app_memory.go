package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"agent-overflow/internal/memory"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

// Memory (docs/specs/memory.md): what a thread's sessions receive as memory
// through one hook, whole at a start and a compaction and what changed at
// each prompt, and the folders they may write.

// sessionMemory is what a session starts with as memory.
type sessionMemory struct {
	// Plan is what the session's hook delivers; nil when the thread
	// receives no memory or the hook could not be built.
	Plan *memory.Plan
	// Hook is the hook that delivers it; nil without a Plan.
	Hook *provider.SessionStartHook
	// Forget forgets the thread's records, so the next prompt delivers the
	// whole memory: the session runs it when a start or a compaction ended
	// without the hook's output, and Codex at each compaction. nil without
	// a hook.
	Forget func() error
	// Dirs are the folders outside the workspace the session may write:
	// the memory folders Prepare vouches for and an assistant's projects.
	// Claude takes them as additional directories, Codex as writable
	// roots.
	Dirs []string
	// Problems are what the caller reports once the start has happened or
	// failed (reportSessionMemory).
	Problems []memoryProblem
	// OwnMemoryOff turns the provider's own memory off in the session
	// (ownMemoryOff).
	OwnMemoryOff bool
	// resumed is set for a session that resumes its provider session: it
	// delivers the whole memory only after a compaction, so its start does
	// not report the delivery's size.
	resumed bool
}

// memoryProblem is a failure of a thread's memory and what it costs the
// thread, which its warning row says.
type memoryProblem struct {
	cost string
	err  error
}

// The costs a memoryProblem names.
const (
	memoryUnread      = "Part of this conversation's memory could not be read, and its sessions receive a notice about it"
	memoryUnprepared  = "A memory folder of this conversation could not be prepared, so its sessions may not write it"
	memoryUndelivered = "This conversation's session started without its memory, because the hook that delivers it could not be set up"
	memoryUnrecorded  = "Agent Overflow could not prepare the folder where it records what this conversation's sessions were shown as memory, so a prompt may deliver the whole memory again"
	memoryUnforgotten = "Agent Overflow could not forget what earlier sessions of this conversation were shown as memory, so a later prompt may deliver only what changed although the model no longer holds the rest"
	memoryOversize    = "Part of this conversation's memory is longer than a provider may show the model of one hook output, so its sessions may see only that part's start and the path of a file holding all of it"
)

// readProblems are failed reads of a delivery as memoryProblems.
func readProblems(errs []error) []memoryProblem {
	problems := make([]memoryProblem, len(errs))
	for i, err := range errs {
		problems[i] = memoryProblem{memoryUnread, err}
	}
	return problems
}

// sizeProblems are the problems of the parts of a delivery over
// memory.Limit, each naming its part.
func sizeProblems(outputs []memory.Output) []memoryProblem {
	var problems []memoryProblem
	for _, output := range outputs {
		if output.Over() {
			problems = append(problems, memoryProblem{memoryOversize, fmt.Errorf("%s is %d bytes, over the %d a hook's output reaches every provider's model whole", output.Name, output.Size, memory.Limit)})
		}
	}
	return problems
}

// receivesMemory reports whether the sessions of a thread receive Agent
// Overflow memory under current: an assistant's current thread always
// does, and any other thread when it works for an assistant (a helper, or
// a thread below one, store.WorksForAssistant of lineage) or while shared
// memory is on. lineage is the thread whose lineage counts, "" for one
// with none on this computer.
func (a *App) receivesMemory(current settings.Settings, lineage string, assistant bool) (bool, error) {
	if a.configDir == "" {
		return false, nil
	}
	if assistant || current.SharedMemoryEnabled {
		return true, nil
	}
	if lineage == "" {
		return false, nil
	}
	return a.store.WorksForAssistant(lineage)
}

// ownMemoryOff reports whether a session of providerName runs with the
// provider's own memory off under current, given whether it receives Agent
// Overflow memory (receivesMemory): a session that does uses that memory,
// whether or not its mode lets it write.
func ownMemoryOff(current settings.Settings, providerName string, receives bool) bool {
	var when string
	switch providerName {
	case string(provider.Claude), string(provider.ClaudeTUI):
		when = current.ClaudeOwnMemoryOff
	case string(provider.Codex):
		when = current.CodexOwnMemoryOff
	default:
		return false
	}
	switch when {
	case settings.OwnMemoryOffAlways:
		return true
	case settings.OwnMemoryOffNever:
		return false
	default:
		return receives
	}
}

// lineageOwnMemoryOff is ownMemoryOff for a session of providerName on a
// thread that is no assistant's current thread and works for lineage: a
// fork (forkOptions.Lineage), or a thread itself, whose walk counts it when
// it is an assistant's current thread or helper (store.WorksForAssistant).
func (a *App) lineageOwnMemoryOff(providerName, lineage string) (bool, error) {
	current := a.currentSettings()
	receives, err := a.receivesMemory(current, lineage, false)
	if err != nil {
		return false, err
	}
	return ownMemoryOff(current, providerName, receives), nil
}

// memoryPlan is the delivery threadID's sessions receive under current; ok
// is false for none (receivesMemory). An assistant's current thread
// receives its home and the projects it serves. Any other thread receives
// its project's memory when it has a project, and the user's.
func (a *App) memoryPlan(current settings.Settings, thread store.Thread, agent *store.StandingAgent) (_ memory.Plan, ok bool, _ error) {
	receives, err := a.receivesMemory(current, thread.ID, agent != nil)
	if err != nil || !receives {
		return memory.Plan{}, false, err
	}
	records, err := memory.RecordsDir(a.configDir, thread.ID)
	if err != nil {
		return memory.Plan{}, false, err
	}
	plan := memory.Plan{User: memory.UserDir(a.configDir), Records: records}
	var projects []store.Project
	switch {
	case agent != nil:
		plan.Home = agent.Home
		served, err := a.store.ServedProjects(agent.ID)
		if err != nil {
			return memory.Plan{}, false, err
		}
		projects = served
	case thread.ProjectID != "":
		project, err := a.store.GetProject(thread.ProjectID)
		if err != nil {
			return memory.Plan{}, false, fmt.Errorf("read the project of %s for its memory: %w", thread.ID, err)
		}
		projects = []store.Project{project}
	}
	// A project's key is its path from the user's home, which an isolated
	// boot pins with the provider homes.
	userHome, err := a.providerHome()
	if err != nil {
		return memory.Plan{}, false, err
	}
	for _, project := range projects {
		dir, err := memory.ProjectDir(a.configDir, userHome, project.Path)
		if err != nil {
			return memory.Plan{}, false, fmt.Errorf("the memory folder of project %s: %w", project.Name, err)
		}
		plan.Projects = append(plan.Projects, memory.Project{Name: project.Name, Path: project.Path, Dir: dir})
	}
	return plan, true, nil
}

// readSessionMemory is what thread's session starts with as memory. agent
// is the assistant whose current thread it is, nil for any other thread;
// resumed is set for a start that resumes its provider session; readOnly is
// set for a session whose runtime mode refuses every write. The start does
// not build the delivery: the hook does, and reportSessionMemory reads it
// for its failures after the start.
func (a *App) readSessionMemory(thread store.Thread, agent *store.StandingAgent, resumed, readOnly bool) (sessionMemory, error) {
	out, err := a.sessionMemoryHook(thread, agent, resumed, readOnly)
	if err != nil {
		return sessionMemory{}, err
	}
	if out.Hook == nil {
		// A compaction in a session with no hook removes what earlier
		// sessions were shown, and nothing records that: a later prompt
		// must not trust their records, whatever kept the hook out.
		if err := a.forgetMemoryRecords(thread.ID); err != nil {
			out.Problems = append(out.Problems, memoryProblem{memoryUnforgotten, err})
		}
	}
	return out, nil
}

// sessionMemoryHook is readSessionMemory before it settles a session with
// no hook: the session's memory, its hook nil when the thread receives no
// memory or the hook could not be built.
func (a *App) sessionMemoryHook(thread store.Thread, agent *store.StandingAgent, resumed, readOnly bool) (sessionMemory, error) {
	current := a.currentSettings()
	plan, ok, err := a.memoryPlan(current, thread, agent)
	if err != nil {
		return sessionMemory{}, fmt.Errorf("read the memory of %s: %w", thread.ID, err)
	}
	out := sessionMemory{OwnMemoryOff: ownMemoryOff(current, thread.Provider, ok), resumed: resumed}
	if !ok {
		return out, nil
	}
	plan.ReadOnly = readOnly
	writable, err := memory.Prepare(plan)
	if err != nil {
		out.Problems = append(out.Problems, memoryProblem{memoryUnprepared, err})
	}
	out.Dirs = writable
	if agent != nil {
		for _, project := range plan.Projects {
			out.Dirs = append(out.Dirs, project.Path)
		}
	}
	if err := memory.PrepareRecords(plan.Records); err != nil {
		out.Problems = append(out.Problems, memoryProblem{memoryUnrecorded, err})
	}
	hook, err := a.memoryHook(thread.ID, plan, os.Executable)
	if err != nil {
		out.Problems = append(out.Problems, memoryProblem{memoryUndelivered, err})
		return out, nil
	}
	out.Plan = &plan
	out.Hook = hook
	out.Forget = func() error { return memory.ForgetRecords(plan.Records) }
	return out, nil
}

// memoryHook is the hook that delivers plan to threadID's session by
// running this executable, which locate finds, once per part of the
// delivery: whole at a start, a compaction and a clear, and what changed at
// each prompt. It writes the plan to the thread's plan file, which every
// command names.
func (a *App) memoryHook(threadID string, plan memory.Plan, locate func() (string, error)) (*provider.SessionStartHook, error) {
	executable, err := locate()
	if err != nil {
		return nil, fmt.Errorf("locate this executable for the hook that delivers memory: %w", err)
	}
	planFile, err := memory.PlanFile(a.configDir, threadID)
	if err != nil {
		return nil, err
	}
	if err := memory.WritePlan(planFile, plan); err != nil {
		return nil, err
	}
	hook := &provider.SessionStartHook{
		Label:   memory.Label,
		Ending:  memory.Ending,
		Stamp:   memory.StampPrefix,
		Failure: memory.HookSubcommand + ":",
	}
	for _, part := range plan.Parts() {
		command, err := memory.HookCommand(executable, memory.AtSessionStart, part, planFile)
		if err != nil {
			return nil, fmt.Errorf("build the SessionStart hook that delivers memory: %w", err)
		}
		prompt, err := memory.HookCommand(executable, memory.AtPrompt, part, planFile)
		if err != nil {
			return nil, fmt.Errorf("build the UserPromptSubmit hook that delivers changes to memory: %w", err)
		}
		hook.Parts = append(hook.Parts, provider.SessionStartPart{Command: command, PromptCommand: prompt, Heading: plan.HeadingOf(part)})
	}
	return hook, nil
}

// forgetMemoryRecords deletes the records of what threadID's sessions were
// shown as memory, so each one's next prompt delivers the whole memory.
func (a *App) forgetMemoryRecords(threadID string) error {
	if a.configDir == "" {
		return nil
	}
	dir, err := memory.RecordsDir(a.configDir, threadID)
	if err != nil {
		return err
	}
	return memory.ForgetRecords(dir)
}

// deleteMemoryFiles deletes the memory files of threadID, which is being
// deleted: the records of what its sessions were shown, and the plan file
// its sessions' hooks read.
func (a *App) deleteMemoryFiles(threadID string) error {
	if a.configDir == "" {
		return nil
	}
	planFile, err := memory.PlanFile(a.configDir, threadID)
	if err != nil {
		return err
	}
	return errors.Join(a.forgetMemoryRecords(threadID), memory.RemovePlan(planFile))
}

// forgetMemoryRecordsAfterCut forgets what threadID's sessions were shown as
// memory after a cut of its history that may have removed it, so the next
// prompt delivers it whole. The cut has happened, so a failure is a warning
// on the thread.
func (a *App) forgetMemoryRecordsAfterCut(threadID string) {
	if err := a.forgetMemoryRecords(threadID); err != nil {
		log.Printf("memory %s: forget the memory records after a cut of the history: %v", threadID, err)
		if err := a.emitNoticeToThread(threadID, noticeKindWarning, "Agent Overflow could not record that this rollback removed memory from the conversation, so the next prompt may deliver only what changed: "+err.Error()+"."); err != nil {
			log.Printf("memory %s: %v", threadID, err)
		}
	}
}

// memoryPromptText renders a session's AO memory plan into plain text suitable
// for providers without SessionStart CLI hooks (ACP / Antigravity).
func memoryPromptText(mem sessionMemory) string {
	if mem.Plan == nil {
		return ""
	}
	outputs, _ := memory.Build(*mem.Plan, time.Now())
	parts := make([]string, 0, len(outputs))
	for _, out := range outputs {
		if text := strings.TrimSpace(out.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// reportSessionMemory reports mem's problems once its session's start has
// happened or failed: the delivery is read for its failures, and a fresh
// start's for its size, off the start's path, as the hook reads it.
func (a *App) reportSessionMemory(threadID string, mem sessionMemory) {
	if mem.Plan == nil {
		a.reportMemoryProblems(threadID, mem.Problems)
		return
	}
	plan := *mem.Plan
	if !a.goMemoryReport(threadID, func() {
		outputs, read := memory.Build(plan, time.Now())
		problems := append(mem.Problems, readProblems(read)...)
		if !mem.resumed {
			problems = append(problems, sizeProblems(outputs)...)
		}
		a.reportMemoryProblems(threadID, problems)
	}) {
		a.reportMemoryProblems(threadID, mem.Problems)
	}
}

// reportMemoryCompactionProblems reports what the memory of a thread whose
// live session compacted cannot read, and its size, as a fresh start does: the
// compaction runs the hook again, with the plan the session started with.
// It reads off the provider event worker.
func (a *App) reportMemoryCompactionProblems(threadID string) {
	live, ok := a.sessionManager().get(threadID)
	if !ok || live.MemoryPlan == nil {
		return
	}
	plan := *live.MemoryPlan
	a.goMemoryReport(threadID, func() {
		outputs, read := memory.Build(plan, time.Now())
		a.reportMemoryProblems(threadID, append(readProblems(read), sizeProblems(outputs)...))
	})
}

// reportMemoryProblems logs each problem of a thread's memory and puts it
// on the thread as a warning row with what it costs: the person watching
// the thread has to know.
func (a *App) reportMemoryProblems(threadID string, problems []memoryProblem) {
	for _, problem := range problems {
		log.Printf("memory %s: %v", threadID, problem.err)
		if err := a.emitNoticeToThread(threadID, noticeKindWarning, problem.cost+": "+problem.err.Error()+"."); err != nil {
			log.Printf("memory %s: %v", threadID, err)
		}
	}
}

// goMemoryReport runs fn for threadID on its own goroutine, joined at
// shutdown before the store closes. False when shutdown has begun and fn
// did not run.
func (a *App) goMemoryReport(threadID string, fn func()) bool {
	return a.memoryReports.wg.Go(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("memory %s: the report panicked: %v\n%s", threadID, recovered, debug.Stack())
				a.emitErrorToThread(threadID, fmt.Sprintf("The app hit an internal error while reading this conversation's memory (%v).", recovered))
			}
		}()
		fn()
	})
}

// waitMemoryReports blocks until the reports already begun have finished,
// without refusing more. It is the observation point for tests.
func (a *App) waitMemoryReports() {
	a.memoryReports.wg.Wait()
}

// stopMemoryReports refuses new reports and waits out the ones in flight.
func (a *App) stopMemoryReports() {
	a.memoryReports.wg.Close()
}
