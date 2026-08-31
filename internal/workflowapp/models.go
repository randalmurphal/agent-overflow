package workflowapp

import (
	"encoding/json"

	"agent-overflow/internal/workflow/memory"
)

type Budget struct {
	Kind          string
	CeilingTokens int64
	CeilingUSD    float64
	CeilingMillis int64
	SpentTokens   int64
	SpentUSD      float64
	ElapsedMillis int64
	Percent       int
	Estimated     bool
	UnpricedRows  int64
	Exhausted     bool
	RootItemID    string
}

type PhaseAttempt struct {
	PhaseID        string
	Attempt        int
	Status         string
	Provider       string
	Model          string
	Effort         string
	Cause          string
	Session        string
	Decision       string
	DecisionTarget string
	ExhaustedLoops []string
	Outputs        []OutputDigest
	OutputOverflow int
}

type OutputDigest struct{ Name, Value string }

type FailedUnit struct {
	UnitID      string
	UnitAttempt int
	Note        string
}

type RunView struct {
	ItemID              string
	WorkflowID          string
	Goal                string
	State               string
	Reason              string
	CurrentPhaseID      string
	CurrentPhaseOrdinal int
	PhaseCount          int
	ParentItemID        string
	Resting             bool
	StartedAt           int64
	EndedAt             int64
	Seeds               json.RawMessage
	FailedUnits         []FailedUnit
	Phases              []PhaseAttempt
	Budget              *Budget
	PendingGuidance     int
}

type RunOutputs struct {
	ItemID    string
	State     string
	Reason    string
	Resting   bool
	Outputs   map[string]any
	Artifacts []string
}

type InspectInput struct {
	ItemID  string
	PhaseID string
	Attempt int
}

type RunInspection struct {
	Run          RunView
	WorktreePath string
	Branch       string
	BaseBranch   string
	Children     []ChildRun
	Guidance     []GuidanceEntry
	Phase        *PhaseDetail
}

type ChildRun struct {
	ItemID, WorkflowID, Goal, State, Reason string
	ParentPhaseID, ParentUnitID             string
	ParentAttempt                           int
}

type GuidanceEntry struct {
	Text, By, ByRun string
	At, AgeSeconds  int64
}

type PhaseDetail struct {
	PhaseID, Status, Provider, Model, Effort, Cause string
	Attempt                                         int
	Outputs                                         map[string]json.RawMessage
	Decision, DecisionTarget                        string
	ExhaustedLoops                                  []string
	Units                                           []UnitView
}

type UnitView struct {
	UnitID, Kind, Status, Note, Branch, WorktreePath, ThreadID string
	UnitAttempt                                                int
}

type NarrativeInput struct {
	ItemID, PhaseID, UnitID string
	Attempt                 int
}

type Narrative struct {
	ItemID, PhaseID, UnitID, Path, Content string
	Attempt, UnitAttempt                   int
	Present                                bool
	Bytes                                  int64
	Truncated                              bool
}

type MemoryInput struct {
	ItemID, Kind, Text string
	Files              []string
}

type MemoryResult struct {
	ItemID, RootID, Kind, Path string
	Wave                       int
}

type MemoryListInput struct{ ItemID, Kind string }

type MemoryLog struct {
	ItemID, RootID, Path string
	Notes                []memory.Note
	Total, Skipped       int
}

type MemoryTree struct {
	RootID, NotesPath string
	Wave              int
}

type WatchInput struct {
	ItemID     string
	Cursor     int64
	Tree       bool
	WaitMillis int64
}

type Transition struct {
	Seq, At          int64
	ItemID, PhaseID  string
	Attempt          int
	From, To, Reason string
	Cause            string
	Resting          bool
}

type WatchRunState struct {
	ItemID, WorkflowID, Goal, State, Reason, PhaseID, Repair string
	Resting                                                  bool
}

type WatchResult struct {
	ItemID      string
	Cursor      int64
	Transitions []Transition
	Run         WatchRunState
	Gap         bool
}
