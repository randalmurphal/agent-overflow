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
// through one SessionStart hook, and the folders they may write.

// sessionMemory is what a session starts with as memory.
type sessionMemory struct {
	// Plan is what the session's hook delivers; nil when the thread
	// receives no memory or the hook could not be built.
	Plan *memory.Plan
	// Hooks hold the one hook that delivers it; none without a Plan.
	Hooks []provider.SessionStartHook
	// Receipts keeps what the hook delivered; nil without a hook.
	Receipts *provider.SessionStartReceipts
	// Dirs are the folders outside the workspace the session may write:
	// the memory folders Prepare vouches for and an assistant's projects.
	// Claude takes them as additional directories, Codex as writable
	// roots.
	Dirs []string
	// Problems are what the caller reports once the start has happened or
	// failed (reportSessionMemory).
	Problems []memoryProblem
	// built is set when the start already built the delivery, a resume
	// deciding whether to deliver it, so its read failures are in
	// Problems.
	built bool
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
	memoryUndelivered = "This conversation's session started without its memory, because the SessionStart hook that delivers it could not be set up"
	memoryUnforgotten = "Agent Overflow could not forget what an earlier session of this conversation received as memory, so a later resume may not deliver it again"
)

// readProblems are failed reads of a delivery as memoryProblems.
func readProblems(errs []error) []memoryProblem {
	problems := make([]memoryProblem, len(errs))
	for i, err := range errs {
		problems[i] = memoryProblem{memoryUnread, err}
	}
	return problems
}

// memoryPlan is the delivery threadID's sessions receive; ok is false for
// none. An assistant's current thread always receives one, with its home
// and the projects it serves; any other thread only while shared memory is
// on, with its project's memory when it has a project.
func (a *App) memoryPlan(thread store.Thread, agent *store.StandingAgent) (_ memory.Plan, ok bool, _ error) {
	if a.configDir == "" {
		return memory.Plan{}, false, nil
	}
	plan := memory.Plan{User: memory.UserDir(a.configDir)}
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
// resumeRef is the provider session the start resumes, "" for a fresh one;
// readOnly is set for a session whose runtime mode refuses every write.
// A fresh start does not build the delivery: the hook does, and
// reportSessionMemory reads it for its failures after the start.
func (a *App) readSessionMemory(thread store.Thread, agent *store.StandingAgent, resumeRef string, readOnly bool) (sessionMemory, error) {
	plan, ok, err := a.memoryPlan(thread, agent)
	if err != nil {
		return sessionMemory{}, fmt.Errorf("read the memory of %s: %w", thread.ID, err)
	}
	var out sessionMemory
	if !ok {
		// A compaction in a session with no hook removes the delivery an
		// earlier one recorded, and nothing records that: a later resume
		// must not trust the receipt.
		if err := a.store.ForgetSessionStartReceipts(thread.ID); err != nil {
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
	hook, err := memoryHook(plan, os.Executable)
	if err != nil {
		out.Problems = append(out.Problems, memoryProblem{memoryUndelivered, err})
		return out, nil
	}
	if resumeRef != "" {
		receipts, err := a.store.SessionStartReceipts(thread.ID, resumeRef)
		if err != nil {
			return sessionMemory{}, fmt.Errorf("read what the memory hook delivered to %s: %w", thread.ID, err)
		}
		resume, problems := resumeDelivers(plan, receipts[memory.Label])
		hook.Resume = resume
		out.Problems = append(out.Problems, readProblems(problems)...)
		out.built = true
	}
	out.Plan = &plan
	out.Hooks = []provider.SessionStartHook{*hook}
	out.Receipts = a.sessionStartReceipts(thread.ID)
	return out, nil
}

// resumeDelivers reports whether a resume runs plan's hook: when the
// delivery now reads otherwise than received, the digest of what the
// thread's model last received. problems are the delivery's failed reads.
func resumeDelivers(plan memory.Plan, received string) (resume bool, problems []error) {
	text, problems := memory.Build(plan, time.Now())
	return received != memory.Digest(text), problems
}

// memoryHook is the SessionStart hook that delivers plan by running this
// executable, which locate finds, at a start, a compaction and a clear.
func memoryHook(plan memory.Plan, locate func() (string, error)) (*provider.SessionStartHook, error) {
	executable, err := locate()
	if err != nil {
		return nil, fmt.Errorf("locate this executable for the SessionStart hook that delivers memory: %w", err)
	}
	command, err := memory.HookCommand(executable, plan)
	if err != nil {
		return nil, fmt.Errorf("build the SessionStart hook that delivers memory: %w", err)
	}
	return &provider.SessionStartHook{
		Label:   memory.Label,
		Command: command,
		Heading: memory.Heading,
		Ending:  memory.Ending,
		Stamp:   memory.StampPrefix,
	}, nil
}

// sessionStartReceipts keeps the digest of what threadID's model received
// from its memory hook (store.SessionStartReceipts).
func (a *App) sessionStartReceipts(threadID string) *provider.SessionStartReceipts {
	return &provider.SessionStartReceipts{
		Keep: func(sessionRef, label, context string) error {
			return a.store.PutSessionStartReceipt(threadID, sessionRef, label, memory.Digest(context))
		},
		Forget: func() error {
			return a.store.ForgetSessionStartReceipts(threadID)
		},
	}
}

// forgetSessionStartReceiptsAfterCut forgets what the memory hook delivered
// to threadID's model after a cut of its history that may have removed it,
// so the next resume delivers it again. The cut has happened, so a failure
// is a warning on the thread.
func (a *App) forgetSessionStartReceiptsAfterCut(threadID string) {
	if err := a.store.ForgetSessionStartReceipts(threadID); err != nil {
		log.Printf("memory %s: forget the memory receipts after a cut of the history: %v", threadID, err)
		if err := a.emitNoticeToThread(threadID, noticeKindWarning, "Agent Overflow could not record that this rollback removed memory a resume delivered, so the next resume may not deliver it again: "+err.Error()+"."); err != nil {
			log.Printf("memory %s: %v", threadID, err)
		}
	}
}

// reportSessionMemory reports mem's problems once its session's start has
// happened or failed. A fresh start's delivery is read for its failures
// off the start's path, as the hook reads it.
func (a *App) reportSessionMemory(threadID string, mem sessionMemory) {
	if mem.Plan == nil || mem.built {
		a.reportMemoryProblems(threadID, mem.Problems)
		return
	}
	plan := *mem.Plan
	if !a.goMemoryReport(threadID, func() {
		_, read := memory.Build(plan, time.Now())
		a.reportMemoryProblems(threadID, append(mem.Problems, readProblems(read)...))
	}) {
		a.reportMemoryProblems(threadID, mem.Problems)
	}
}

// reportMemoryCompactionProblems reports what the memory of a thread whose
// live session compacted cannot read, as its session start does: the
// compaction runs the hook again, with the plan the session started with.
// It reads off the provider event worker.
func (a *App) reportMemoryCompactionProblems(threadID string) {
	live, ok := a.sessionManager().get(threadID)
	if !ok || live.MemoryPlan == nil {
		return
	}
	plan := *live.MemoryPlan
	a.goMemoryReport(threadID, func() {
		_, read := memory.Build(plan, time.Now())
		a.reportMemoryProblems(threadID, readProblems(read))
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
