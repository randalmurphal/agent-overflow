package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/harness/control"
)

// claudeTaskLedger follows the background tasks this process announced,
// off every frame it wrote (scenario emits included), so an inbound
// interrupt answers with the terminals the real CLI writes for them.
//
// Modeled on the 2.1.280 stream-json captures behind claude-wire.md
// §Background task ownership (2026-09-24, captures A, B and D). A
// `control_request{interrupt}`:
//
//   - kills every running async agent: the level set without it, then
//     `task_updated{killed}`, then `task_notification{stopped}` naming
//     its output file;
//   - kills a PARKED agent too (it reported and stopped while a shell it
//     started still runs): `task_updated{killed}` and no notification;
//   - kills each background shell a killed agent owns: the level set
//     without it, `task_updated{killed}`, `task_notification{stopped}`;
//   - is then acknowledged with `{"still_queued":[]}`;
//   - stops a foreground command still running, the main thread's or an
//     agent's, with `task_notification{stopped}` carrying
//     `output_file:""` and no `task_updated`, after the ack;
//   - leaves the main thread's own background shells running.
//
// The CLI writes an agent's shell terminals on either side of the ack
// (before it in capture D, after it in A). AO reads each frame on its
// own, so this ledger writes every kill before the ack and every
// foreground stop after it.
//
// A task's ownership comes from the frames: a shell belongs to the agent
// whose launch `tool_use` its own `tool_use` hangs under
// (`parent_tool_use_id`). A task is background when its `task_started`
// says `is_backgrounded` or a `background_tasks_changed` level set lists
// it; the CLI announces every background task both ways, and the
// scenario helpers rely on the level set.
type claudeTaskLedger struct {
	mu    sync.Mutex
	order []string
	tasks map[string]*claudeTask
	// parentOf maps a tool_use id to the parent_tool_use_id its assistant
	// frame carried.
	parentOf map[string]string
	// killedAgents counts the agents the last interrupt killed, for the
	// interrupted result's subagent_stats.
	killedAgents int
	// tasksDir is where a killed shell's output file is written, so the
	// app's read of it finds a file the way it does with the CLI.
	tasksDir string
}

type claudeTask struct {
	id, toolUseID, taskType, description string
	background                           bool
	parentToolUseID                      string
	// stopped is set by task_updated{completed|failed}: the task reported.
	// A stopped agent is parked while a shell it owns still runs.
	stopped bool
	killed  bool
}

func newClaudeTaskLedger() *claudeTaskLedger {
	home := strings.TrimSpace(os.Getenv(control.EnvTranscriptHome))
	if home == "" {
		home = os.TempDir()
	}
	return &claudeTaskLedger{
		tasks:    make(map[string]*claudeTask),
		parentOf: make(map[string]string),
		tasksDir: filepath.Join(home, "ao-mock-tasks", strconv.Itoa(os.Getpid())),
	}
}

// observe reads one written frame. Called under the writer lock, in wire
// order, for scenario frames and the ledger's own alike; the kill frames
// re-mark what killForInterrupt already marked.
func (l *claudeTaskLedger) observe(line string) {
	if !strings.Contains(line, `"tool_use"`) && !strings.Contains(line, `"task_`) && !strings.Contains(line, `"background_tasks_changed"`) {
		return
	}
	var env struct {
		Type            string `json:"type"`
		Subtype         string `json:"subtype"`
		TaskID          string `json:"task_id"`
		ToolUseID       string `json:"tool_use_id"`
		TaskType        string `json:"task_type"`
		Description     string `json:"description"`
		IsBackgrounded  bool   `json:"is_backgrounded"`
		Status          string `json:"status"`
		ParentToolUseID string `json:"parent_tool_use_id"`
		Patch           struct {
			Status string `json:"status"`
		} `json:"patch"`
		Tasks []struct {
			TaskID      string `json:"task_id"`
			TaskType    string `json:"task_type"`
			Description string `json:"description"`
		} `json:"tasks"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &env) != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case env.Type == "assistant":
		var blocks []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(env.Message.Content, &blocks) != nil {
			return
		}
		for _, block := range blocks {
			if block.Type == "tool_use" && block.ID != "" {
				l.parentOf[block.ID] = env.ParentToolUseID
			}
		}
	case env.Type != "system":
		return
	case env.Subtype == "task_started":
		if env.TaskID == "" {
			return
		}
		t := l.taskLocked(env.TaskID)
		if env.ToolUseID != "" {
			t.toolUseID = env.ToolUseID
			t.parentToolUseID = l.parentOf[env.ToolUseID]
		}
		if env.TaskType != "" {
			t.taskType = env.TaskType
		}
		if env.Description != "" {
			t.description = env.Description
		}
		t.background = t.background || env.IsBackgrounded
		// A wake (claude-wire.md §E6b) is a task_started for a parked
		// agent: it runs again.
		t.stopped = false
		t.killed = false
	case env.Subtype == "task_updated":
		t := l.tasks[env.TaskID]
		if t == nil {
			return
		}
		switch env.Patch.Status {
		case "completed", "failed":
			t.stopped = true
		case "killed":
			t.killed = true
		}
	case env.Subtype == "task_notification":
		t := l.tasks[env.TaskID]
		if t == nil {
			return
		}
		switch env.Status {
		case "stopped", "killed":
			t.killed = true
		}
	case env.Subtype == "background_tasks_changed":
		for _, listed := range env.Tasks {
			if listed.TaskID == "" {
				continue
			}
			t := l.taskLocked(listed.TaskID)
			t.background = true
			if t.taskType == "" {
				t.taskType = listed.TaskType
			}
			if t.description == "" {
				t.description = listed.Description
			}
		}
	}
}

func (l *claudeTaskLedger) taskLocked(id string) *claudeTask {
	t := l.tasks[id]
	if t == nil {
		t = &claudeTask{id: id}
		l.tasks[id] = t
		l.order = append(l.order, id)
	}
	return t
}

// killForInterrupt marks every agent an interrupt kills, with the
// background shells they own, and returns the frames that report it, in
// the order the CLI writes them. The caller writes them before the ack.
func (l *claudeTaskLedger) killForInterrupt(now time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.killedAgents = 0
	endTime := now.UnixMilli()
	var frames []string
	levelSet := l.levelSetLocked()
	emitLevelSet := func() {
		next := l.levelSetLocked()
		if next == levelSet {
			return
		}
		levelSet = next
		frames = append(frames, next)
	}
	for _, id := range l.order {
		agent := l.tasks[id]
		if agent.taskType != "local_agent" || !agent.background || agent.killed {
			continue
		}
		if agent.stopped && !l.ownsLiveShellLocked(agent) {
			continue
		}
		wasRunning := !agent.stopped
		agent.killed = true
		l.killedAgents++
		emitLevelSet()
		frames = append(frames, taskUpdatedKilledFrame(agent.id, endTime))
		if wasRunning {
			frames = append(frames, taskNotificationStoppedFrame(agent, l.outputFilePath(agent.id)))
		}
		for _, shellID := range l.order {
			shell := l.tasks[shellID]
			if !l.liveShellOwnedByLocked(shell, agent) {
				continue
			}
			shell.killed = true
			emitLevelSet()
			frames = append(frames,
				taskUpdatedKilledFrame(shell.id, endTime),
				taskNotificationStoppedFrame(shell, l.writeOutputFile(shell.id)))
		}
	}
	return frames
}

// stopForegroundCommands marks every foreground command still running,
// the main thread's or an agent's, and returns their stopped
// notifications. The caller writes them after the ack.
func (l *claudeTaskLedger) stopForegroundCommands() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var frames []string
	for _, id := range l.order {
		t := l.tasks[id]
		if t.taskType == "local_agent" || t.background || t.stopped || t.killed || t.toolUseID == "" {
			continue
		}
		t.killed = true
		frames = append(frames, taskNotificationStoppedFrame(t, ""))
	}
	return frames
}

// lastKilledAgents reports how many agents the last interrupt killed.
func (l *claudeTaskLedger) lastKilledAgents() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.killedAgents
}

func (l *claudeTaskLedger) ownsLiveShellLocked(agent *claudeTask) bool {
	for _, id := range l.order {
		if l.liveShellOwnedByLocked(l.tasks[id], agent) {
			return true
		}
	}
	return false
}

func (l *claudeTaskLedger) liveShellOwnedByLocked(shell, agent *claudeTask) bool {
	return agent.toolUseID != "" &&
		shell.parentToolUseID == agent.toolUseID &&
		shell.taskType != "local_agent" &&
		shell.background && !shell.stopped && !shell.killed
}

// levelSetLocked is the CLI's `background_tasks_changed` frame for the
// tasks still running in the background: a parked agent is not in it.
func (l *claudeTaskLedger) levelSetLocked() string {
	tasks := make([]map[string]string, 0, len(l.order))
	for _, id := range l.order {
		t := l.tasks[id]
		if !t.background || t.stopped || t.killed {
			continue
		}
		tasks = append(tasks, map[string]string{
			"task_id":     t.id,
			"task_type":   t.taskType,
			"description": t.description,
		})
	}
	return mustJSON(map[string]any{
		"type":    "system",
		"subtype": "background_tasks_changed",
		"tasks":   tasks,
	})
}

func (l *claudeTaskLedger) outputFilePath(taskID string) string {
	return filepath.Join(l.tasksDir, taskID+".output")
}

// writeOutputFile creates the empty output file a killed shell's
// notification names. The CLI's holds the command's output so far; the
// mock ran no command, and the app's read of the file must still find
// one. A write failure is logged and the path still named, so the read
// fails on the app's side the way a missing file would with the CLI.
func (l *claudeTaskLedger) writeOutputFile(taskID string) string {
	path := l.outputFilePath(taskID)
	if err := os.MkdirAll(l.tasksDir, 0o755); err != nil {
		log.Printf("claude: create task output dir %s: %v", l.tasksDir, err)
		return path
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		log.Printf("claude: write task output %s: %v", path, err)
	}
	return path
}

func taskUpdatedKilledFrame(taskID string, endTime int64) string {
	return mustJSON(map[string]any{
		"type":    "system",
		"subtype": "task_updated",
		"task_id": taskID,
		"patch":   map[string]any{"status": "killed", "end_time": endTime},
	})
}

func taskNotificationStoppedFrame(t *claudeTask, outputFile string) string {
	return mustJSON(map[string]any{
		"type":        "system",
		"subtype":     "task_notification",
		"task_id":     t.id,
		"tool_use_id": t.toolUseID,
		"status":      "stopped",
		"output_file": outputFile,
		"summary":     t.description,
	})
}
