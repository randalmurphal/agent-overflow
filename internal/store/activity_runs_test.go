package store

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

const (
	goActivityRunVectorsPath = "testdata/activity_run_vectors.json"
	tsActivityRunVectorsPath = "../../frontend/src/test/fixtures/activityRunVectors.json"
)

type activityRunVectorFile struct {
	Note  string              `json:"note"`
	Cases []activityRunVector `json:"cases"`
}

type activityRunVector struct {
	Name  string                  `json:"name"`
	Why   string                  `json:"why"`
	Rules []string                `json:"rules"`
	Items []activityRunVectorItem `json:"items"`
	Runs  []activityRunVectorRun  `json:"runs"`
}

// activityRunVectorRules maps a vector's rule names to the store's rules.
var activityRunVectorRules = map[string]ActivityRunRule{
	"agent-message-members":   AgentMessagesAreMembers,
	"agent-messages-absorbed": AgentMessagesAbsorbed,
}

type activityRunVectorItem struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ToolName     string `json:"toolName"`
	Status       string `json:"status"`
	CompletionOf string `json:"completionOf"`
	PayloadKind  string `json:"payloadKind"`
	IsBackground bool   `json:"isBackground"`
	Meta         string `json:"meta"`
	PayloadMeta  string `json:"payloadMeta"`
}

type activityRunVectorRun struct {
	FirstItemID string                  `json:"firstItemId"`
	LastItemID  string                  `json:"lastItemId"`
	MemberIDs   []string                `json:"memberIds"`
	Stubs       []activityRunVectorStub `json:"stubs"`
}

type activityRunVectorStub struct {
	ShippedIDs                 []string             `json:"shippedIds"`
	MemberCount                int                  `json:"memberCount"`
	LoadedFirstItemID          string               `json:"loadedFirstItemId"`
	LoadedLastItemID           string               `json:"loadedLastItemId"`
	UnshippedBefore            int                  `json:"unshippedBefore"`
	UnshippedAfter             int                  `json:"unshippedAfter"`
	UnshippedGroups            []ActivityRunGroup   `json:"unshippedGroups"`
	UnshippedPairedLaunchIDs   []string             `json:"unshippedPairedLaunchIds"`
	ShippedSupersededLaunchIDs []string             `json:"shippedSupersededLaunchIds"`
	UnshippedFailed            bool                 `json:"unshippedFailed"`
	RunningBefore              *ActivityRunGroupKey `json:"runningBefore"`
	RunningAfter               *ActivityRunGroupKey `json:"runningAfter"`
}

// TestActivityRunVectors is one half of a cross-language contract: the
// server's run classification and stub aggregate must equal what the
// client computes from the same rows, or a collapsed run's header would
// report one number while the rows behind it say another. The vectors run
// against a real store, so the SQL that reads payload kind, meta.mcp and
// the three file-change sources is part of what they pin.
func TestActivityRunVectors(t *testing.T) {
	file := loadActivityRunVectors(t)
	if len(file.Cases) == 0 {
		t.Fatal("vector file carries no cases")
	}
	ruled := map[ActivityRunRule]int{}
	for _, vector := range file.Cases {
		if len(vector.Rules) == 0 {
			t.Fatalf("%s names no rule", vector.Name)
		}
		for _, name := range vector.Rules {
			rule, ok := activityRunVectorRules[name]
			if !ok {
				t.Fatalf("%s names unknown rule %q", vector.Name, name)
			}
			ruled[rule]++
			t.Run(vector.Name+"/"+name, func(t *testing.T) {
				s := newTestStore(t)
				seedActivityRunVector(t, s, "t", vector)
				units := allTimelineUnits(t, s, "t", rule)
				assertVectorRuns(t, vector, units)
				assertVectorStubs(t, vector, units)
			})
		}
	}
	for name, rule := range activityRunVectorRules {
		if ruled[rule] == 0 {
			t.Errorf("no vector holds under %s", name)
		}
	}
}

// TestActivityRunVectorsAreSharedByteForByte keeps the two copies one
// file, for the reason the digest vectors are kept that way: the frontend
// suite cannot read out of internal/, and a duplicate nobody compares is a
// fixture that drifts and takes the contract with it.
func TestActivityRunVectorsAreSharedByteForByte(t *testing.T) {
	goCopy, err := os.ReadFile(goActivityRunVectorsPath)
	if err != nil {
		t.Fatalf("read Go vectors: %v", err)
	}
	tsCopy, err := os.ReadFile(tsActivityRunVectorsPath)
	if err != nil {
		t.Fatalf("read TypeScript vectors: %v", err)
	}
	if string(goCopy) != string(tsCopy) {
		t.Fatalf("%s and %s differ; they must be byte-identical",
			goActivityRunVectorsPath, tsActivityRunVectorsPath)
	}
}

func loadActivityRunVectors(t *testing.T) activityRunVectorFile {
	t.Helper()
	raw, err := os.ReadFile(goActivityRunVectorsPath)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var file activityRunVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	return file
}

// seedActivityRunVector writes a case's rows as one turn of visible
// top-level history, in fixture order.
func seedActivityRunVector(t *testing.T, s *Store, threadID string, vector activityRunVector) {
	t.Helper()
	if err := s.CreateThread(makeThread(threadID, "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for i, row := range vector.Items {
		item := Item{
			ID:           row.ID,
			ThreadID:     threadID,
			TurnIndex:    0,
			ItemIndex:    i,
			Kind:         row.Kind,
			Role:         vectorItemRole(row.Kind),
			Status:       row.Status,
			Summary:      row.ID,
			ToolName:     row.ToolName,
			CompletionOf: row.CompletionOf,
			IsBackground: row.IsBackground,
			Meta:         row.Meta,
			CreatedAt:    int64(i + 1),
		}
		if row.PayloadKind == "" && row.PayloadMeta == "" {
			if err := s.InsertItem(item); err != nil {
				t.Fatalf("insert %s: %v", row.ID, err)
			}
			continue
		}
		payload := Payload{
			ID:        "p-" + row.ID,
			Kind:      row.PayloadKind,
			Meta:      row.PayloadMeta,
			Data:      []byte("body"),
			CreatedAt: int64(i + 1),
		}
		item.PayloadID = payload.ID
		if err := s.InsertItemWithPayload(item, payload); err != nil {
			t.Fatalf("insert %s with payload: %v", row.ID, err)
		}
	}
}

func vectorItemRole(kind string) string {
	switch kind {
	case "user_text":
		return "user"
	case "notification":
		return "system"
	default:
		return "assistant"
	}
}

// allTimelineUnits walks a whole thread through the production unit
// walker, oldest first.
func allTimelineUnits(t *testing.T, s *Store, threadID string, rule ActivityRunRule) []pageUnit {
	t.Helper()
	walk := newActivityScanWalk(s.reader(), threadID, timelineTailBound(), false, timelineScope{}, rule)
	var units []pageUnit
	for {
		unit, found, err := nextOlderUnit(walk)
		if err != nil {
			t.Fatalf("walk units: %v", err)
		}
		if !found {
			break
		}
		units = append(units, unit)
	}
	for i, j := 0, len(units)-1; i < j; i, j = i+1, j-1 {
		units[i], units[j] = units[j], units[i]
	}
	return units
}

func assertVectorRuns(t *testing.T, vector activityRunVector, units []pageUnit) {
	t.Helper()
	var got []activityRunVectorRun
	for _, unit := range units {
		if !unit.run {
			continue
		}
		members := make([]string, 0, len(unit.rows))
		for _, row := range unit.rows {
			members = append(members, row.ID)
		}
		got = append(got, activityRunVectorRun{
			FirstItemID: unit.oldest().ID,
			LastItemID:  unit.newest().ID,
			MemberIDs:   members,
		})
	}
	if len(got) != len(vector.Runs) {
		t.Fatalf("classified %d runs, want %d: got %v", len(got), len(vector.Runs), got)
	}
	for i, want := range vector.Runs {
		if got[i].FirstItemID != want.FirstItemID || got[i].LastItemID != want.LastItemID {
			t.Errorf("run %d edges = %s..%s, want %s..%s",
				i, got[i].FirstItemID, got[i].LastItemID, want.FirstItemID, want.LastItemID)
		}
		if !reflect.DeepEqual(got[i].MemberIDs, want.MemberIDs) {
			t.Errorf("run %d members = %v, want %v", i, got[i].MemberIDs, want.MemberIDs)
		}
	}
}

func assertVectorStubs(t *testing.T, vector activityRunVector, units []pageUnit) {
	t.Helper()
	runUnits := map[string]pageUnit{}
	for _, unit := range units {
		if unit.run {
			runUnits[unit.oldest().ID] = unit
		}
	}
	for _, wantRun := range vector.Runs {
		unit, ok := runUnits[wantRun.FirstItemID]
		if !ok {
			continue // assertVectorRuns already reported it
		}
		for _, want := range wantRun.Stubs {
			from, to := vectorShippedSpan(t, unit.rows, want.ShippedIDs)
			got := buildActivityRunStub(unit.rows, from, to)
			assertVectorStub(t, wantRun.FirstItemID, want, got, unit.rows, from, to)
		}
	}
}

// vectorShippedSpan resolves a case's shipped ids to the half-open span
// over the run's members, and fails when they are not a contiguous run of
// members: a page never ships a gap, so a fixture that states one is
// describing a page that cannot exist.
func vectorShippedSpan(t *testing.T, rows []activityScanRow, shipped []string) (int, int) {
	t.Helper()
	if len(shipped) == 0 {
		return 0, 0
	}
	from := -1
	for i, row := range rows {
		if row.ID == shipped[0] {
			from = i
			break
		}
	}
	if from < 0 {
		t.Fatalf("shipped id %s is not a member of the run", shipped[0])
	}
	for offset, id := range shipped {
		if from+offset >= len(rows) || rows[from+offset].ID != id {
			t.Fatalf("shipped ids %v are not a contiguous member span", shipped)
		}
	}
	return from, from + len(shipped)
}

func assertVectorStub(
	t *testing.T,
	runID string,
	want activityRunVectorStub,
	got ActivityRunStub,
	rows []activityScanRow,
	from, to int,
) {
	t.Helper()
	label := func(field string) string {
		return runID + " shipped " + got.LoadedFirstItemID + ".." + got.LoadedLastItemID + " " + field
	}
	if got.MemberCount != want.MemberCount {
		t.Errorf("%s = %d, want %d", label("memberCount"), got.MemberCount, want.MemberCount)
	}
	if got.LoadedFirstItemID != want.LoadedFirstItemID || got.LoadedLastItemID != want.LoadedLastItemID {
		t.Errorf("%s = %s..%s, want %s..%s", label("loaded span"),
			got.LoadedFirstItemID, got.LoadedLastItemID,
			want.LoadedFirstItemID, want.LoadedLastItemID)
	}
	if got.UnshippedBefore != want.UnshippedBefore || got.UnshippedAfter != want.UnshippedAfter {
		t.Errorf("%s = %d/%d, want %d/%d", label("unshipped before/after"),
			got.UnshippedBefore, got.UnshippedAfter, want.UnshippedBefore, want.UnshippedAfter)
	}
	wantGroups := want.UnshippedGroups
	if wantGroups == nil {
		wantGroups = []ActivityRunGroup{}
	}
	if !reflect.DeepEqual(got.UnshippedGroups, wantGroups) {
		t.Errorf("%s = %+v, want %+v", label("unshippedGroups"), got.UnshippedGroups, wantGroups)
	}
	wantPaired := want.UnshippedPairedLaunchIDs
	if wantPaired == nil {
		wantPaired = []string{}
	}
	if !reflect.DeepEqual(got.UnshippedPairedLaunchIDs, wantPaired) {
		t.Errorf("%s = %v, want %v", label("unshippedPairedLaunchIds"),
			got.UnshippedPairedLaunchIDs, wantPaired)
	}
	wantSuperseded := want.ShippedSupersededLaunchIDs
	if wantSuperseded == nil {
		wantSuperseded = []string{}
	}
	if !reflect.DeepEqual(got.ShippedSupersededLaunchIDs, wantSuperseded) {
		t.Errorf("%s = %v, want %v", label("shippedSupersededLaunchIds"),
			got.ShippedSupersededLaunchIDs, wantSuperseded)
	}
	if got.UnshippedFailed != want.UnshippedFailed {
		t.Errorf("%s = %v, want %v", label("unshippedFailed"), got.UnshippedFailed, want.UnshippedFailed)
	}
	if !reflect.DeepEqual(got.RunningBefore, want.RunningBefore) {
		t.Errorf("%s = %+v, want %+v", label("runningBefore"), got.RunningBefore, want.RunningBefore)
	}
	if !reflect.DeepEqual(got.RunningAfter, want.RunningAfter) {
		t.Errorf("%s = %+v, want %+v", label("runningAfter"), got.RunningAfter, want.RunningAfter)
	}

	// The digest is not in the fixture — it folds a per-row revision the
	// client cannot predict — but it has to be exactly the unshipped rows.
	unshipped := make([]WindowDigestRow, 0, len(rows))
	for i, row := range rows {
		if i >= from && i < to {
			continue
		}
		unshipped = append(unshipped, WindowDigestRow{ID: row.ID, Rev: row.Rev})
	}
	if wantDigest := WindowDigest(unshipped); got.UnshippedDigest != wantDigest {
		t.Errorf("%s = %s, want %s over %d rows",
			label("unshippedDigest"), got.UnshippedDigest, wantDigest, len(unshipped))
	}
}

func TestActivityRunStubIgnoresSettledBackgroundLaunch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	threadID := "t-settled-bg"
	if err := s.CreateThread(makeThread(threadID, "agy")); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Background launch stays status='running' forever in history while its
	// completion landed in a later activity run after a steered user message,
	// stamping live_background_active=false on the launch row.
	if err := s.InsertItem(Item{
		ID:           "bg-launch-1",
		ThreadID:     threadID,
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		ToolName:     "Bash",
		Meta:         `{"live_background_active":false}`,
		CreatedAt:    1000,
		UpdatedAt:    2000,
	}); err != nil {
		t.Fatalf("InsertItem settled launch: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:           "bg-launch-2",
		ThreadID:     threadID,
		TurnIndex:    1,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		ToolName:     "Bash",
		Meta:         `{"live_background_active":true}`,
		CreatedAt:    3000,
		UpdatedAt:    3000,
	}); err != nil {
		t.Fatalf("InsertItem live launch: %v", err)
	}

	rows, err := queryActivityScanRows(s.reader(), threadID, 2, fixedIDSelection(`SELECT id FROM items WHERE thread_id = ?`, threadID))
	if err != nil {
		t.Fatalf("queryActivityScanRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if !rows[0].BackgroundSettled || rows[0].isRunningStatus() {
		t.Fatalf("settled launch row should have BackgroundSettled=true and isRunningStatus=false: %+v", rows[0])
	}
	if rows[1].BackgroundSettled || !rows[1].isRunningStatus() {
		t.Fatalf("live launch row should have BackgroundSettled=false and isRunningStatus=true: %+v", rows[1])
	}

	stubSettled := buildActivityRunStub(rows[:1], 0, 0)
	if stubSettled.RunningBefore != nil || stubSettled.RunningAfter != nil {
		t.Fatalf("settled launch stub must report nil RunningBefore/After, got before=%+v after=%+v", stubSettled.RunningBefore, stubSettled.RunningAfter)
	}
	stubLive := buildActivityRunStub(rows[1:], 0, 0)
	if stubLive.RunningBefore == nil || stubLive.RunningBefore.ToolName != "Bash" {
		t.Fatalf("live launch stub must report RunningBefore=Bash, got %+v", stubLive.RunningBefore)
	}
}
