package app

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"time"

	"agent-overflow/internal/memory"
	"agent-overflow/internal/provider"
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
	memoryOversize    = "This conversation's memory is longer than a provider shows the model whole, so its sessions see its start and the path of a file holding all of it"
)

// readProblems are failed reads of a delivery as memoryProblems.
func readProblems(errs []error) []memoryProblem {
	problems := make([]memoryProblem, len(errs))
	for i, err := range errs {
		problems[i] = memoryProblem{memoryUnread, err}
	}
	return problems
}

// sizeProblems is the problem of a delivery over memory.Limit, none for one
// under it.
func sizeProblems(text string) []memoryProblem {
	if size, over := memory.Oversize(text); over {
		return []memoryProblem{{memoryOversize, fmt.Errorf("%d bytes, over the %d a hook's output reaches the model whole", size, memory.Limit)}}
	}
	return nil
}

// memoryPlan is the delivery threadID's sessions receive; ok is false for
// none. An assistant's current thread always receives one, with its home
// and the projects it serves; any other thread only while shared memory is
// on, with its project's memory when it has a project.
func (a *App) memoryPlan(thread store.Thread, agent *store.StandingAgent) (_ memory.Plan, ok bool, _ error) {
	if a.configDir == "" {
		return memory.Plan{}, false, nil
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
	case !a.currentSettings().SharedMemoryEnabled:
		return memory.Plan{}, false, nil
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
	plan, ok, err := a.memoryPlan(thread, agent)
	if err != nil {
		return sessionMemory{}, fmt.Errorf("read the memory of %s: %w", thread.ID, err)
	}
	out := sessionMemory{resumed: resumed}
	if !ok {
		// A compaction in a session with no hook removes what earlier
		// sessions were shown, and nothing records that: a later prompt
		// must not trust their records.
		if err := a.forgetMemoryRecords(thread.ID); err != nil {
			out.Problems = append(out.Problems, memoryProblem{memoryUnforgotten, err})
		}
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
	hook, err := memoryHook(plan, os.Executable)
	if err != nil {
		out.Problems = append(out.Problems, memoryProblem{memoryUndelivered, err})
		return out, nil
	}
	out.Plan = &plan
	out.Hook = hook
	out.Forget = func() error { return memory.ForgetRecords(plan.Records) }
	return out, nil
}

// memoryHook is the hook that delivers plan by running this executable,
// which locate finds: whole at a start, a compaction and a clear, and what
// changed at each prompt.
func memoryHook(plan memory.Plan, locate func() (string, error)) (*provider.SessionStartHook, error) {
	executable, err := locate()
	if err != nil {
		return nil, fmt.Errorf("locate this executable for the hook that delivers memory: %w", err)
	}
	command, err := memory.HookCommand(executable, memory.AtSessionStart, plan)
	if err != nil {
		return nil, fmt.Errorf("build the SessionStart hook that delivers memory: %w", err)
	}
	prompt, err := memory.HookCommand(executable, memory.AtPrompt, plan)
	if err != nil {
		return nil, fmt.Errorf("build the UserPromptSubmit hook that delivers changes to memory: %w", err)
	}
	return &provider.SessionStartHook{
		Label:         memory.Label,
		Command:       command,
		PromptCommand: prompt,
		Heading:       memory.Heading,
		Ending:        memory.Ending,
		Stamp:         memory.StampPrefix,
		Failure:       memory.HookSubcommand + ":",
	}, nil
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
		text, read := memory.Build(plan, time.Now())
		problems := append(mem.Problems, readProblems(read)...)
		if !mem.resumed {
			problems = append(problems, sizeProblems(text)...)
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
		text, read := memory.Build(plan, time.Now())
		a.reportMemoryProblems(threadID, append(readProblems(read), sizeProblems(text)...))
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
	w := &a.memoryReports
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("memory %s: the report panicked: %v\n%s", threadID, recovered, debug.Stack())
				a.emitErrorToThread(threadID, fmt.Sprintf("The app hit an internal error while reading this conversation's memory (%v).", recovered))
			}
		}()
		fn()
	}()
	return true
}

// waitMemoryReports blocks until the reports already begun have finished,
// without refusing more. It is the observation point for tests.
func (a *App) waitMemoryReports() {
	a.memoryReports.wg.Wait()
}

// stopMemoryReports refuses new reports and waits out the ones in flight.
func (a *App) stopMemoryReports() {
	a.memoryReports.mu.Lock()
	a.memoryReports.stopped = true
	a.memoryReports.mu.Unlock()
	a.memoryReports.wg.Wait()
}
