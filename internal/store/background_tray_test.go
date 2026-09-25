package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// seedParkedTrayAgents adds background agents in every run state to
// thread "t": running; parked at its newest stop past an older one a wake
// followed; woken after its stop; parked on a run that wrote no report;
// done past a parked stop; ended; a resume carrier parked at its own
// sibling while its root runs; a retired parked agent with a running
// foreground agent under it, and a retired parked agent under a running
// one, neither of which the tray serves. Every stop is inside the
// retention window, where an ending sibling is a tray row.
// Indexes start at 20, past seedTrayFixture's.
func seedParkedTrayAgents(t *testing.T, s *Store) {
	t.Helper()
	index := 20
	insert := func(item Item, payload *Payload) {
		t.Helper()
		item.ThreadID, item.TurnIndex, item.ItemIndex = "t", 1, index
		if item.Role == "" {
			item.Role = "assistant"
		}
		if item.CreatedAt == 0 {
			item.CreatedAt = 1000
		}
		index++
		var err error
		if payload != nil {
			err = insertWithPayloadCarded(s, item, *payload)
		} else {
			err = insertCarded(s, item)
		}
		if err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}
	agent := func(id, tool, meta string) {
		insert(Item{ID: id, Kind: "tool_call", Status: "running", Summary: "Agent: " + id, ToolName: tool,
			IsBackground: true, Meta: meta}, nil)
	}
	child := func(id, parent, tool, meta string, background bool) {
		insert(Item{ID: id, Kind: "tool_call", Status: "running", Summary: tool + ": " + id, ToolName: tool,
			ParentID: parent, IsBackground: background, Meta: meta}, nil)
	}
	report := func(id, parent, text string) {
		insert(Item{ID: id, Kind: "assistant_text", Status: "completed", Summary: text, ParentID: parent}, nil)
	}
	stop := func(launchID string, createdAt int64, run map[string]any, preview string) {
		t.Helper()
		id := "complete:" + launchID + ":parked:" + strconv.FormatInt(createdAt, 10)
		meta, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		item := Item{ID: id, Kind: "tool_completion", Status: ItemStatusParked, Summary: "parked", ToolName: "Agent",
			IsBackground: true, CompletionOf: launchID, Meta: string(meta), CreatedAt: createdAt}
		if preview == "" {
			insert(item, nil)
			return
		}
		payloadMeta, err := json.Marshal(map[string]any{"outputFileState": "loaded", "preview": preview})
		if err != nil {
			t.Fatal(err)
		}
		item.PayloadID = "tool-call-result:" + id
		insert(item, &Payload{ID: item.PayloadID, Kind: "tool_call_result", Meta: string(payloadMeta), CreatedAt: createdAt})
	}
	wake := func(rootID string, createdAt int64) {
		insert(Item{ID: "wake:" + rootID + ":" + strconv.FormatInt(createdAt, 10), Kind: "user_text", Role: "user",
			Status: "completed", Summary: "Background command done", ParentID: rootID,
			Meta: `{"wire_only":true,"` + wakePromptMetaKey + `":true}`, CreatedAt: createdAt}, nil)
	}
	completion := func(launchID, meta string) {
		insert(Item{ID: "complete:" + launchID, Kind: "tool_completion", Status: "completed", Summary: "done",
			ToolName: "Agent", IsBackground: true, CompletionOf: launchID, Meta: meta, CreatedAt: 6000}, nil)
	}
	run := func(commands int, reportID string) map[string]any {
		out := map[string]any{MetaKeyParkedCommands: commands, MetaKeyRunStartedAt: 1000}
		if reportID != "" {
			out[MetaKeyParkedReportItemID] = reportID
		}
		return out
	}

	agent("agent-running", "Agent", `{"task_id":"task-running"}`)
	child("running-retired", "agent-running", "Agent", `{"task_id":"task-running-retired","live_background_active":false}`, true)
	report("running-retired-report", "running-retired", "Waiting.")
	stop("running-retired", 6000, run(1, "running-retired-report"), "Waiting.")

	agent("agent-parked", "Agent", `{"task_id":"task-parked"}`)
	report("parked-report-1", "agent-parked", "First report.")
	stop("agent-parked", 6000, run(5, "parked-report-1"), "First report.")
	wake("agent-parked", 6000)
	child("parked-shell", "agent-parked", "Bash", `{}`, true)
	report("parked-report-2", "agent-parked", "Waiting on the gate.")
	stop("agent-parked", 6001, run(2, "parked-report-2"), "Waiting on the gate.")

	agent("agent-woken", "Agent", `{"task_id":"task-woken"}`)
	stop("agent-woken", 6000, run(1, ""), "")
	wake("agent-woken", 6002)

	agent("agent-silent", "Agent", `{"task_id":"task-silent"}`)
	stop("agent-silent", 6000, run(1, ""), "The stop's own words.")

	agent("agent-done", "Task", `{"task_id":"task-done"}`)
	stop("agent-done", 5500, run(1, ""), "")
	completion("agent-done", `{"status_source":"task_notification"}`)
	agent("agent-ended", "Agent", `{"task_id":"task-ended"}`)
	completion("agent-ended", `{"status_source":"session_died"}`)

	agent("carrier-root", "Agent", `{"task_id":"task-carried"}`)
	child("carrier-shell", "carrier-root", "Bash", `{}`, true)
	report("carrier-report", "carrier-root", "Round two report")
	agent("carrier", "SendMessage", `{"task_id":"task-carrier","transcript_root_id":"carrier-root","description":"Spike"}`)
	stop("carrier", 6000, run(1, "carrier-report"), "Round two report")

	agent("agent-retired", "Agent", `{"task_id":"task-retired","live_background_active":false}`)
	stop("agent-retired", 6000, run(1, ""), "")
	child("retired-inner", "agent-retired", "Agent", `{}`, false)
	child("retired-inner-read", "retired-inner", "Read", `{}`, false)

	child("shell-alone", "", "Bash", `{"task_id":"task-shell"}`, true)
}

// servedRunStates reads each launch row's served run-state keys.
func servedRunStates(t *testing.T, items []Item) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, item := range items {
		if item.CompletionOf != "" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
			t.Fatalf("decode %s meta: %v", item.ID, err)
		}
		state := map[string]any{}
		for key, value := range meta {
			if strings.HasPrefix(key, "subagentRunState") || strings.HasPrefix(key, "subagentParked") {
				state[key] = value
			}
		}
		out[item.ID] = state
	}
	return out
}

// completionRowIDs lists the completion rows of a tray read.
func completionRowIDs(items []Item) []string {
	var ids []string
	for _, item := range items {
		if item.CompletionOf != "" {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// The tray read serves each background agent's run state from its stops,
// read by its own statement: an ending sibling settles it (ended after a
// session death); its newest stop parks it when that stop is a parked
// sibling no wake under its transcript root followed, serving what the
// sibling recorded; otherwise it runs. A parked sibling is not a tray
// row, and neither seeds nor keeps a retired launch. A background shell
// has no run state.
func TestListLiveBackgroundTasksServesTheRunStates(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedParkedTrayAgents(t, s)
	items, err := s.ListLiveBackgroundTasks("t", 5000)
	if err != nil {
		t.Fatal(err)
	}
	got := servedRunStates(t, items)
	parked := func(waiting int, reportID, preview string) map[string]any {
		return map[string]any{
			MetaKeySubagentRunState: AgentRunParked, MetaKeySubagentParkedCommands: float64(waiting),
			MetaKeySubagentParkedReportID: reportID, MetaKeySubagentParkedReportPreview: preview,
		}
	}
	want := map[string]map[string]any{
		"agent-running": {MetaKeySubagentRunState: AgentRunRunning},
		"agent-parked":  parked(2, "parked-report-2", "Waiting on the gate."),
		"parked-shell":  {},
		"agent-woken":   {MetaKeySubagentRunState: AgentRunRunning},
		"agent-silent":  {MetaKeySubagentRunState: AgentRunParked, MetaKeySubagentParkedCommands: float64(1)},
		"agent-done":    {MetaKeySubagentRunState: AgentRunDone},
		"agent-ended":   {MetaKeySubagentRunState: AgentRunEnded},
		"carrier-root":  {MetaKeySubagentRunState: AgentRunRunning},
		"carrier-shell": {},
		"carrier":       parked(1, "carrier-report", "Round two report"),
		"shell-alone":   {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("served run states:\n got  %v\n want %v", got, want)
	}
	if ids := completionRowIDs(items); !slices.Equal(ids, []string{"complete:agent-done", "complete:agent-ended"}) {
		t.Errorf("completion rows = %v, want the two ending siblings", ids)
	}
}

// The tray serves the parked predicate Store.CurrentParkedStop reads, and
// what the stop it finds recorded, for every agent in both reads.
func TestTrayRunStateMatchesCurrentParkedStop(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedParkedTrayAgents(t, s)
	whole, err := s.ListLiveBackgroundTasks("t", 5000)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, item := range whole {
		if item.CompletionOf != "" || !IsAgentTranscriptLaunch(item) {
			continue
		}
		named, err := s.ListBackgroundTrayRows("t", 5000, []string{item.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range append([]Item{item}, named...) {
			if row.ID != item.ID {
				continue
			}
			root := item.ID
			if stamped := transcriptRootOf(t, item); stamped != "" {
				root = stamped
			}
			stop, parked, err := s.CurrentParkedStop("t", item.ID, root)
			if err != nil {
				t.Fatal(err)
			}
			state := servedRunStates(t, []Item{row})[row.ID]
			if servedParked := state[MetaKeySubagentRunState] == AgentRunParked; servedParked != parked {
				t.Errorf("%s served %v, CurrentParkedStop parked=%v", row.ID, state, parked)
				continue
			}
			checked++
			if !parked {
				continue
			}
			var recorded map[string]any
			if err := json.Unmarshal([]byte(stop.Meta), &recorded); err != nil {
				t.Fatal(err)
			}
			if state[MetaKeySubagentParkedCommands] != recorded[MetaKeyParkedCommands] ||
				state[MetaKeySubagentParkedReportID] != recorded[MetaKeyParkedReportItemID] {
				t.Errorf("%s served %v, its parked stop %s recorded %v", row.ID, state, stop.ID, recorded)
			}
			if reportID, _ := recorded[MetaKeyParkedReportItemID].(string); reportID != "" {
				var payload map[string]any
				if err := json.Unmarshal([]byte(stop.PayloadMeta), &payload); err != nil {
					t.Fatal(err)
				}
				if state[MetaKeySubagentParkedReportPreview] != payload["preview"] {
					t.Errorf("%s served preview %v, its stop's payload %v", row.ID, state[MetaKeySubagentParkedReportPreview], payload["preview"])
				}
			}
		}
	}
	if checked != 16 {
		t.Errorf("compared %d served agent rows, want the 8 agents in both reads", checked)
	}
}

func transcriptRootOf(t *testing.T, item Item) string {
	t.Helper()
	var meta struct {
		Root string `json:"transcript_root_id"`
	}
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatal(err)
	}
	return meta.Root
}

// A tray delta is the whole read's rows for the launches it names and the
// carriers stamped with a named root: the same rows, in the same order,
// served the same, for every launch alone and for all of them at once.
func TestListBackgroundTrayRowsMatchesTheWholeRead(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	cutoff := seedTrayFixture(t, s)
	seedParkedTrayAgents(t, s)
	whole, err := s.ListLiveBackgroundTasks("t", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT id FROM items WHERE thread_id = 't' AND kind = 'tool_call' ORDER BY item_index`)
	if err != nil {
		t.Fatal(err)
	}
	var launches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		launches = append(launches, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	launches = append(launches, "ghost", "missing")

	carriersOf := func(root string) []string {
		t.Helper()
		rows, err := s.db.Query(`SELECT id FROM items WHERE thread_id = 't' AND json_extract(meta, '$.transcript_root_id') = ?`, root)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	for _, id := range launches {
		named := append([]string{id}, carriersOf(id)...)
		want := slices.DeleteFunc(slices.Clone(whole), func(item Item) bool {
			return !slices.Contains(named, item.ID) && !slices.Contains(named, item.CompletionOf)
		})
		got, err := s.ListBackgroundTrayRows("t", cutoff, []string{id})
		if err != nil {
			t.Fatalf("rows of %s: %v", id, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the rows of %s:\n got  %v\n want %v", id, collectIDs(got), collectIDs(want))
		}
	}
	all, err := s.ListBackgroundTrayRows("t", cutoff, launches)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all, whole) {
		t.Errorf("every launch by name:\n got  %v\n want %v", collectIDs(all), collectIDs(whole))
	}
	if none, err := s.ListBackgroundTrayRows("t", cutoff, nil); err != nil || len(none) != 0 {
		t.Errorf("no launch named: %v, %v", collectIDs(none), err)
	}
}

// A tray delta costs what its named launches cost: its statement reads
// none of the indexes that enumerate the thread's background tasks, and
// reads no parent's children outside a parked launch's facts.
func TestListBackgroundTrayRowsReadsOnlyTheNamedLaunches(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedTrayFixture(t, s)
	nodes := trayPlan(t, s, backgroundTrayRowsSQL, "t", int64(5000), `["live","nested-agent"]`)
	carriersByIndex := false
	for _, node := range nodes {
		carriersByIndex = carriersByIndex || strings.Contains(node.detail, "SEARCH carrier USING INDEX idx_items_transcript_root (thread_id=? AND <expr>=?)")
		for _, enumerates := range []string{
			"idx_items_running_bg_tool_calls",
			"idx_items_running_nested_fg_tool_calls",
			"idx_items_completion_created",
			"idx_items_thread_turn_item_unique",
		} {
			if strings.Contains(node.detail, enumerates) {
				t.Errorf("a tray delta reads %s: %q", enumerates, node.detail)
			}
		}
	}
	if !carriersByIndex {
		t.Error("a tray delta finds a named root's carriers without a keyed probe of idx_items_transcript_root")
	}
	assertTrayPlanReadsNoChildren(t, nodes, "asked", "named", "up", "anchors", "cand", "json_each")
}

// A carrier's run state reads the wakes filed under its transcript root,
// so a delta that names the root serves the carrier too: a wake there
// runs the parked carrier again.
func TestListBackgroundTrayRowsServesARootsParkedCarrier(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedParkedTrayAgents(t, s)
	carrierState := func() any {
		t.Helper()
		rows, err := s.ListBackgroundTrayRows("t", 5000, []string{"carrier-root"})
		if err != nil {
			t.Fatal(err)
		}
		if got := collectIDs(rows); !reflect.DeepEqual(got, []string{"carrier-root", "carrier"}) {
			t.Fatalf("a delta naming the root returns %v", got)
		}
		return servedRunStates(t, rows)["carrier"][MetaKeySubagentRunState]
	}
	if got := carrierState(); got != AgentRunParked {
		t.Fatalf("the carrier is served %v, want parked", got)
	}
	if err := insertCarded(s, Item{ID: "carrier-wake", ThreadID: "t", TurnIndex: 1, ItemIndex: 90,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "Background command done", ParentID: "carrier-root",
		Meta: `{"wire_only":true,"` + wakePromptMetaKey + `":true}`, CreatedAt: 6001}); err != nil {
		t.Fatalf("wake the carrier: %v", err)
	}
	if got := carrierState(); got != AgentRunRunning {
		t.Errorf("after a wake under its root the carrier is served %v, want running", got)
	}
}

// The tray statements bind only the thread, the cutoff and the named
// launches. Their LIMITs are literal: SQLite recompiles a statement with
// a bound LIMIT each time new values are bound, and the tray reads run on
// every tray write.
func TestTrayStatementsBindNoLimit(t *testing.T) {
	boundLimit := regexp.MustCompile(`(?i)\bLIMIT\s+[?:@$]`)
	for name, query := range map[string]string{
		"liveBackgroundTasksSQL": liveBackgroundTasksSQL,
		"backgroundTrayRowsSQL":  backgroundTrayRowsSQL,
	} {
		if boundLimit.MatchString(query) {
			t.Errorf("%s binds a LIMIT", name)
		}
		if !strings.Contains(query, "LIMIT 1") {
			t.Errorf("%s does not limit a launch's newest stop to one row", name)
		}
	}
}

// A launch's newest stop is one step of idx_items_completion_of, sorted by
// the index, and a wake is a range probe of idx_items_subagent_wake: in
// both tray statements and in the reads CurrentParkedStop runs.
func TestTrayRunStateProbesItsIndexes(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedParkedTrayAgents(t, s)
	for name, plan := range map[string][]trayPlanNode{
		"liveBackgroundTasksSQL": trayPlan(t, s, liveBackgroundTasksSQL, "t", int64(5000)),
		"backgroundTrayRowsSQL":  trayPlan(t, s, backgroundTrayRowsSQL, "t", int64(5000), `["agent-parked"]`),
		"newestStopSQL":          trayPlan(t, s, newestStopSQL, "t", "agent-parked"),
		"latestWakeSQL":          trayPlan(t, s, latestWakeSQL, "t", "agent-parked", int64(6000)),
	} {
		assertTrayRunStateProbes(t, name, plan, name != "latestWakeSQL", name != "newestStopSQL")
	}
}

// assertTrayRunStateProbes checks a plan's newest-stop probe (alias s) and
// wake probe (alias w).
func assertTrayRunStateProbes(t *testing.T, name string, nodes []trayPlanNode, stop, wake bool) {
	t.Helper()
	var plan strings.Builder
	for _, node := range nodes {
		plan.WriteString(node.detail)
		plan.WriteString("\n")
	}
	found := map[string]bool{}
	for _, node := range nodes {
		switch {
		case strings.HasPrefix(node.detail, "SEARCH s "):
			found["stop"] = node.detail == "SEARCH s USING INDEX idx_items_completion_of (thread_id=? AND completion_of=?)"
			for _, sibling := range nodes {
				if sibling.parent == node.parent && strings.Contains(sibling.detail, "TEMP B-TREE") {
					t.Errorf("%s sorts a launch's stops: %q\n%s", name, sibling.detail, plan.String())
				}
			}
		case strings.HasPrefix(node.detail, "SEARCH w "):
			found["wake"] = node.detail == "SEARCH w USING INDEX idx_items_subagent_wake (thread_id=? AND parent_id=? AND created_at>?)"
		}
	}
	if found["stop"] != stop || found["wake"] != wake {
		t.Errorf("%s probes stop=%v wake=%v, want stop=%v wake=%v:\n%s", name, found["stop"], found["wake"], stop, wake, plan.String())
	}
}
