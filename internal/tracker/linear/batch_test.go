package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// batchID is an invented issue ID in Linear's spelling.
func batchID(i int) string { return fmt.Sprintf("6f1c2a3b-0000-4000-8000-%012x", 0x100+i) }

func batchIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = batchID(i)
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// renumberVars are the variables of a renumbering of ids to values.
func renumberVars(ids []string, values []float64) string {
	v := map[string]any{}
	for i, id := range ids {
		v[fmt.Sprintf("id%d", i)] = id
		v[fmt.Sprintf("input%d", i)] = map[string]float64{"sortOrder": values[i]}
	}
	return mustJSON(v)
}

// renumberAnswer answers each alias with its issue at its value.
func renumberAnswer(ids []string, values []float64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf(`"i%d":{"success":true,"value":%s}`, i, lineartest.IssueJSON(id, fmt.Sprintf("WEB-%d", i+1), "2026-11-02T10:00:00.000Z", values[i]))
	}
	return `{"data":{` + strings.Join(parts, ",") + `}}`
}

// readBack is the read of ids that settles a failed write, answered with
// issues at sortOrders (a missing ID is not listed).
func readBack(ids []string, sortOrders map[string]float64, cycles map[string]string) lineartest.Exchange {
	var nodes []string
	for i, id := range ids {
		so, ok := sortOrders[id]
		if !ok {
			continue
		}
		is := lineartest.NewIssue(id, fmt.Sprintf("WEB-%d", i+1), "2026-11-02T10:00:00.000Z", so)
		is.Cycle = cycles[id]
		nodes = append(nodes, is.JSON())
	}
	vars := mustJSON(map[string]any{"first": len(ids), "includeArchived": true, "sort": []any{map[string]any{"manual": map[string]string{"order": "Ascending"}}}, "filter": map[string]any{"and": []any{map[string]any{"id": map[string]any{"in": ids}}}}})
	return lineartest.Exchange{Op: "Issues", Vars: vars, Body: lineartest.Data(lineartest.PageJSON(nodes, false, ""))}
}

// A run longer than maxBatch is written as aliased updates, maxBatch to a
// request, each issue its own part holding its value.
func TestRenumberIssuesChunks(t *testing.T) {
	t.Parallel()
	ids := batchIDs(maxBatch + 2)
	values, err := Spread(f64(0), f64(1000), len(ids))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t,
		lineartest.Exchange{Op: "RenumberIssues", Vars: renumberVars(ids[:maxBatch], values[:maxBatch]), Body: renumberAnswer(ids[:maxBatch], values[:maxBatch])},
		lineartest.Exchange{Op: "RenumberIssues", Vars: renumberVars(ids[maxBatch:], values[maxBatch:]), Body: renumberAnswer(ids[maxBatch:], values[maxBatch:])},
	)
	ws, err := f.client(t).RenumberIssues(context.Background(), RenumberInput{Field: OrderSort, IssueIDs: ids, Before: f64(0), After: f64(1000)})
	if err != nil || len(ws) != len(ids) {
		t.Fatalf("renumber %d parts, %v", len(ws), err)
	}
	for i, w := range ws {
		if w.IssueID != ids[i] || w.Err != nil || w.Issue == nil || w.Issue.SortOrder != values[i] {
			t.Errorf("part %d: %+v", i, w)
		}
	}
}

// A failed request proves nothing about its parts: one read of its
// issues settles each, applied when it holds its value, not applied when
// it holds another or is not listed; the chunks after it are not sent.
func TestRenumberIssuesFailedChunkIsReadBack(t *testing.T) {
	t.Parallel()
	ids := batchIDs(maxBatch + 1)
	values, err := Spread(f64(0), f64(1000), len(ids))
	if err != nil {
		t.Fatal(err)
	}
	chunk := ids[:maxBatch]
	held := map[string]float64{}
	for i, id := range chunk[:maxBatch-1] {
		held[id] = values[i] + 0.25 // unchanged
	}
	held[chunk[1]] = values[1] // applied despite the failure
	delete(held, chunk[2])     // not listed
	f := newFake(t,
		lineartest.Exchange{Op: "RenumberIssues", Vars: renumberVars(chunk, values[:maxBatch]),
			Body: `{"data":null,"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INPUT_ERROR","userError":true}}]}`},
		readBack(chunk, held, nil),
	)
	ws, err := f.client(t).RenumberIssues(context.Background(), RenumberInput{Field: OrderSort, IssueIDs: ids, Before: f64(0), After: f64(1000)})
	if err != nil || len(ws) != len(ids) {
		t.Fatalf("renumber %d parts, %v", len(ws), err)
	}
	if e, ok := errors.AsType[*UnchangedError](ws[0].Err); !ok || !e.Listed || !WriteNotApplied(ws[0].Err) {
		t.Errorf("unchanged part: %v", ws[0].Err)
	}
	if ws[1].Err != nil || ws[1].Issue.SortOrder != values[1] {
		t.Errorf("applied part: %+v", ws[1])
	}
	if e, ok := errors.AsType[*UnchangedError](ws[2].Err); !ok || e.Listed {
		t.Errorf("unlisted part: %v", ws[2].Err)
	}
	if _, ok := errors.AsType[*trackerapi.NotSentError](ws[maxBatch].Err); !ok {
		t.Errorf("part after the failed chunk: %v", ws[maxBatch].Err)
	}
}

// A failed write whose read fails too leaves every part it did not prove
// unknown, never refused.
func TestRenumberIssuesUnknownWhenReadFails(t *testing.T) {
	t.Parallel()
	ids := batchIDs(2)
	values, err := Spread(f64(0), f64(10), 2)
	if err != nil {
		t.Fatal(err)
	}
	read := readBack(ids, nil, nil)
	read.Status, read.Body = 500, `{"errors":[{"message":"boom"}]}`
	f := newFake(t,
		lineartest.Exchange{Op: "RenumberIssues", Vars: renumberVars(ids, values), Status: 500, Body: `{"errors":[{"message":"boom"}]}`},
		read,
	)
	ws, err := f.client(t).RenumberIssues(context.Background(), RenumberInput{Field: OrderSort, IssueIDs: ids, Before: f64(0), After: f64(10)})
	if err != nil || len(ws) != 2 {
		t.Fatalf("renumber %+v %v", ws, err)
	}
	for _, w := range ws {
		e, ok := errors.AsType[*OutcomeUnknownError](w.Err)
		if !ok || WriteNotApplied(w.Err) || e.Write == nil || e.Read == nil {
			t.Errorf("part %s: %v", w.IssueID, w.Err)
		}
	}
}

// Linear's rate limit refuses a write before applying anything, so its
// parts carry that error without a read.
func TestRenumberIssuesRateLimited(t *testing.T) {
	t.Parallel()
	ids := batchIDs(2)
	values, err := Spread(nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t, lineartest.Exchange{Op: "RenumberIssues", Vars: renumberVars(ids, values), Status: 400,
		Body: `{"errors":[{"message":"Rate limit exceeded","extensions":{"type":"ratelimited","code":"RATELIMITED","userError":true}}]}`})
	ws, err := f.client(t).RenumberIssues(context.Background(), RenumberInput{Field: OrderSort, IssueIDs: ids})
	if err != nil || len(ws) != 2 {
		t.Fatalf("renumber %+v %v", ws, err)
	}
	for _, w := range ws {
		if _, ok := errors.AsType[*trackerapi.RateLimitedError](w.Err); !ok {
			t.Errorf("part %s: %v", w.IssueID, w.Err)
		}
	}
}

func setCycleVars(ids []string, cycle any) string {
	return mustJSON(map[string]any{"ids": ids, "input": map[string]any{"cycleId": cycle}})
}

func setCycleAnswer(ids []string, cycle string) string {
	nodes := make([]string, len(ids))
	for i, id := range ids {
		is := lineartest.NewIssue(id, fmt.Sprintf("WEB-%d", i+1), "2026-11-02T10:00:00.000Z", 1)
		is.Cycle = cycle
		nodes[i] = is.JSON()
	}
	return `{"data":{"result":{"success":true,"value":[` + strings.Join(nodes, ",") + `]}}}`
}

// SetCycle sends one batch update per maxBatch issues; an answer that
// shows an issue in another cycle is settled by a read; an empty cycle
// clears it; anything but distinct issue IDs is refused before any
// request.
func TestSetCycle(t *testing.T) {
	t.Parallel()
	ids := batchIDs(maxBatch + 1)
	two := batchIDs(2)
	f := newFake(t,
		lineartest.Exchange{Op: "UpdateIssues", Vars: setCycleVars(ids[:maxBatch], "cycle-2"), Body: setCycleAnswer(ids[:maxBatch], "cycle-2")},
		lineartest.Exchange{Op: "UpdateIssues", Vars: setCycleVars(ids[maxBatch:], "cycle-2"), Body: setCycleAnswer(ids[maxBatch:], "cycle-2")},
		lineartest.Exchange{Op: "UpdateIssues", Vars: setCycleVars(two, nil), Body: setCycleAnswer(two, "cycle-3")},
		readBack(two, map[string]float64{two[0]: 1, two[1]: 1}, map[string]string{two[0]: "", two[1]: "cycle-3"}),
	)
	c := f.client(t)
	ctx := context.Background()
	ws, err := c.SetCycle(ctx, ids, "cycle-2")
	if err != nil || len(ws) != len(ids) {
		t.Fatalf("set %d parts, %v", len(ws), err)
	}
	for i, w := range ws {
		if w.Err != nil || w.Issue.CycleID != "cycle-2" || w.IssueID != ids[i] {
			t.Errorf("part %d: %+v", i, w)
		}
	}
	ws, err = c.SetCycle(ctx, two, "")
	if err != nil || len(ws) != 2 || ws[0].Err != nil || !WriteNotApplied(ws[1].Err) {
		t.Fatalf("clear %+v %v", ws, err)
	}
	for name, bad := range map[string][]string{"identifier": {"WEB-1"}, "twice": {two[0], two[0]}} {
		if _, err := c.SetCycle(ctx, bad, "cycle-2"); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A run with Into moves its first Moved issues into that cycle in the
// same update as their value, the others only renumbered; a moved part
// is applied only when its issue holds the cycle too. Moved without Into,
// or past the run, sends nothing.
func TestRenumberIssuesCarriesTheCycle(t *testing.T) {
	t.Parallel()
	ids := batchIDs(2)
	values, err := Spread(f64(0), f64(10), 2)
	if err != nil {
		t.Fatal(err)
	}
	cycle := "00000000-0000-4000-8000-0000000000c1"
	vars := mustJSON(map[string]any{
		"id0": ids[0], "input0": map[string]any{"sortOrder": values[0], "cycleId": cycle},
		"id1": ids[1], "input1": map[string]any{"sortOrder": values[1]},
	})
	moved := lineartest.NewIssue(ids[0], "WEB-1", "2026-11-02T10:00:00.000Z", values[0])
	moved.Cycle = cycle
	body := `{"data":{"i0":{"success":true,"value":` + moved.JSON() + `},"i1":{"success":true,"value":` + lineartest.IssueJSON(ids[1], "WEB-2", "2026-11-02T10:00:00.000Z", values[1]) + `}}}`
	f := newFake(t, lineartest.Exchange{Op: "RenumberIssues", Vars: vars, Body: body})
	c := f.client(t)
	ws, err := c.RenumberIssues(context.Background(), RenumberInput{Field: OrderSort, IssueIDs: ids, Before: f64(0), After: f64(10), Into: &cycle, Moved: 1})
	if err != nil || len(ws) != 2 || ws[0].Err != nil || ws[0].Issue.CycleID != cycle || ws[1].Err != nil {
		t.Fatalf("renumber %+v, %v", ws, err)
	}
	for _, bad := range []RenumberInput{
		{Field: OrderSort, IssueIDs: ids, Moved: 1},
		{Field: OrderSort, IssueIDs: ids, Into: &cycle},
		{Field: OrderSort, IssueIDs: ids, Into: &cycle, Moved: 3},
	} {
		if _, err := c.RenumberIssues(context.Background(), bad); err == nil {
			t.Errorf("%+v was sent", bad)
		}
	}
}
