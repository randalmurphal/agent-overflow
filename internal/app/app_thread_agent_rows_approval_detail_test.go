package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// What an approval asks quotes the command it would run, which only a
// session that may answer approvals is shown (provider:approval). A
// session that may read threads and nothing more sees that a worker waits
// on an approval and where it went, not what it asks. A question's text is
// thread content, which the same session reads.

// windowCalls is the call of a view-only window on app, a session that may
// read threads and not answer approvals, and of a window that may do both.
func windowCalls(t *testing.T, app *App) (viewOnly, full context.Context) {
	t.Helper()
	if app.identityState() == nil {
		if err := app.initIdentity("window-under-test"); err != nil {
			t.Fatal(err)
		}
	}
	view := pairSessionWithScopes(t, app, "thumb-view-only", []identity.Scope{identity.ScopeThreadsRead})
	whole := pairSessionWithScopes(t, app, "thumb-full", identity.Scopes)
	return callFrom(view.ID, false), callFrom(whole.ID, false)
}

// readRowsAs reads the rows of threadID's Agents listing named by tokens as
// ctx's window does, through the listing and, for a local row, through the
// row read too. A paired computer's answer nests only in the listing.
func readRowsAs(t *testing.T, app *App, ctx context.Context, threadID string, local bool, tokens ...string) map[string][]ThreadAgentRow {
	t.Helper()
	listing, err := app.ListThreadAgentRows(ctx, threadID)
	if err != nil {
		t.Fatalf("ListThreadAgentRows: %v", err)
	}
	out := map[string][]ThreadAgentRow{}
	for _, token := range tokens {
		row, ok := agentRowByToken(listing.Rows, token)
		if !ok {
			t.Fatalf("row %s is not listed", token)
		}
		out[token] = []ThreadAgentRow{row}
	}
	if !local {
		return out
	}
	reads, err := app.GetThreadAgentRows(ctx, threadID, tokens)
	if err != nil {
		t.Fatalf("GetThreadAgentRows: %v", err)
	}
	for i, token := range tokens {
		if !reads[i].Listed || reads[i].Row == nil {
			t.Fatalf("row %s: read %+v", token, reads[i])
		}
		out[token] = append(out[token], *reads[i].Row)
	}
	return out
}

// wantPromptRows asserts each read of an approval row and a question row:
// the approval with its detail only when withDetail, the command text
// nowhere in the row; the question with its text.
func wantPromptRows(t *testing.T, who string, approval, question []ThreadAgentRow, command string, withDetail bool) {
	t.Helper()
	for _, row := range approval {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if row.BlockedOn != "approval" {
			t.Fatalf("%s: the approval row = %+v; want it blocked on an approval", who, row)
		}
		if shown := strings.Contains(string(encoded), command); shown != withDetail || (row.BlockedDetail != "") != withDetail {
			t.Fatalf("%s: the approval row = %s; want its command shown %v", who, encoded, withDetail)
		}
	}
	for _, row := range question {
		if row.BlockedOn != "question" || row.BlockedDetail != "Which module?" {
			t.Fatalf("%s: the question row = %+v; want the question", who, row)
		}
	}
}

// A view-only window reads a local worker's approval without its command,
// and its question in full; a full window and the spawner's own thread
// tools read both.
func TestAViewOnlyWindowSeesALocalWorkersApprovalWithoutItsCommand(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	approval := f.runningRequest(t, "deploy it")
	question := f.runningRequest(t, "ask about it")
	askOn(t, f.app, approval.ThreadID, "approval-1", false)
	askOn(t, f.app, question.ThreadID, "question-1", true)
	viewOnly, full := windowCalls(t, f.app)

	for _, read := range []struct {
		who        string
		ctx        context.Context
		withDetail bool
	}{
		{"a view-only window", viewOnly, false},
		{"a full window", full, true},
		{"the spawner's thread tools", t.Context(), true},
	} {
		rows := readRowsAs(t, f.app, read.ctx, f.caller.ID, true, approval.Token, question.Token)
		wantPromptRows(t, read.who, rows[approval.Token], rows[question.Token], "make deploy", read.withDetail)
	}
}

// A view-only window reads a paired computer's worker's approval without
// its command, though that computer reported it to this one, and its
// question in full.
func TestAViewOnlyWindowSeesAPairedWorkersApprovalWithoutItsCommand(t *testing.T) {
	t.Parallel()
	pair := newAgentPeerPair(t, func(t *testing.T, app *App) *pairedBackend { return servePairedApp(t, app) })
	second := pair.spawnThere(t, "more over there", map[string]any{"notify": true})
	routed := pair.receipt(t, second.Token).TargetThreadID
	routeUnheld(pair.dest)
	if _, err := pair.dest.store.SetThreadPlacement([]string{pair.child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	askOn(t, pair.dest, pair.child, "approval-1", false)
	askOn(t, pair.dest, routed, "question-1", true)
	pair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		pair.poll(t)
		first, _ := agentRowByToken(rows.Rows, pair.spawn.Token)
		next, _ := agentRowByToken(rows.Rows, second.Token)
		return first.BlockedDetail != "" && next.BlockedDetail != ""
	})
	viewOnly, full := windowCalls(t, pair.source)

	for _, read := range []struct {
		who        string
		ctx        context.Context
		withDetail bool
	}{
		{"a view-only window", viewOnly, false},
		{"a full window", full, true},
	} {
		rows := readRowsAs(t, pair.source, read.ctx, pair.caller.ID, false, pair.spawn.Token, second.Token)
		wantPromptRows(t, read.who, rows[pair.spawn.Token], rows[second.Token], "make deploy", read.withDetail)
	}
}

// A paired computer's agent_rows answer to a session that may not answer
// approvals says its worker waits on an approval, not what it asks, and
// says what a question asks; the pairing's own session reads both.
func TestAPeerSessionWithoutApprovalsReadsNoApprovalCommand(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		line, on, command string
	}{
		// The provider titles this approval, which is what its detail
		// quotes.
		"approval": {remoteApproveLine, "approval", "Allow Bash?"},
		"question": {remoteAskLine, "question", "Which module?"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newRemotePromptPair(t, c.line)
			p.openThere(t, 1)
			encoded, err := json.Marshal(threadAgentPeerArgs{ThreadID: p.worker, Spawn: p.spawn.Token, Depth: 1})
			if err != nil {
				t.Fatal(err)
			}
			reply, err := p.dest.ThreadToolQuery(narrowSessionThere(t, p), ThreadPeerCall{
				Tool: threadAgentPeerRows, Args: encoded, Source: threadtools.Caller{ThreadID: p.caller.ID},
			})
			if err != nil {
				t.Fatalf("agent_rows on a session that may read threads: %v", err)
			}
			var listing ThreadAgentRows
			if err := json.Unmarshal(reply.Result, &listing); err != nil {
				t.Fatal(err)
			}
			withDetail := c.on == "question"
			if listing.BlockedOn != c.on || strings.Contains(string(reply.Result), c.command) != withDetail {
				t.Fatalf("the narrow session's answer = %s; want blocked on %s, its text shown %v", reply.Result, c.on, withDetail)
			}
			own, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
			if err != nil || own.BlockedOn != c.on || !strings.Contains(own.BlockedDetail, c.command) {
				t.Fatalf("the pairing's own answer = %+v, %v; want what its prompt asks", own, err)
			}
		})
	}
}
