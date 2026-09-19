package threadtools

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
)

// fakeApp is one computer. A cross-computer test builds two of them and
// wires a peer to the other's Server, so the stamping and grouping paths
// run against real handler output rather than a hand-written reply.
type fakeApp struct {
	name string

	threads  map[string]Thread
	live     map[string]LiveState
	items    map[string][]Item
	payloads map[string][]byte
	kinds    map[string]string
	hits     []Hit
	indexing bool
	catalog  Catalog

	computers []Computer
	peers     map[string]Peer
	peerErr   map[string]error

	movedTo     string
	movedToName string
	movedRefs   []string

	// Canned write results and the calls that produced them.
	ack       RequestAck
	replyAck  ReplyAck
	status    StatusReport
	listing   RequestListing
	cancelled CancelReport
	updated   []ThreadUpdateResult
	groupRes  GroupReport
	export    ExportFile

	spawns  []SpawnCall
	sends   []SendCall
	asks    []AskCall
	replies []ReplyCall
	states  []StatusCall
	lists   []ListCall
	cancels []CancelCall
	rem     []RemindCall
	updates []UpdateCall
	gcalls  []GroupCall

	searches []SearchQuery
	windows  []WindowQuery
	slices   []TranscriptQuery
	reads    []PayloadQuery

	// Hooks for the cases the defaults cannot express.
	onSearch     func(SearchQuery) (SearchPage, error)
	onTranscript func(TranscriptQuery) (TranscriptSlice, error)
	onThread     func(string) (Thread, error)
	failWith     error
}

func newFakeApp(name string) *fakeApp {
	return &fakeApp{
		name:     name,
		threads:  map[string]Thread{},
		live:     map[string]LiveState{},
		items:    map[string][]Item{},
		payloads: map[string][]byte{},
		kinds:    map[string]string{},
		peers:    map[string]Peer{},
		peerErr:  map[string]error{},
	}
}

func (f *fakeApp) addThread(thread Thread) *fakeApp {
	f.threads[thread.ID] = thread
	return f
}

func (f *fakeApp) addItems(threadID string, items ...Item) *fakeApp {
	f.items[threadID] = append(f.items[threadID], items...)
	return f
}

func (f *fakeApp) addPayload(threadID, itemID, kind string, body []byte) *fakeApp {
	f.payloads[threadID+"/"+itemID] = body
	f.kinds[threadID+"/"+itemID] = kind
	return f
}

func (f *fakeApp) Thread(_ context.Context, threadID string) (Thread, error) {
	if f.onThread != nil {
		return f.onThread(threadID)
	}
	thread, ok := f.threads[threadID]
	if !ok {
		return Thread{}, publicf(CodeNotFound, "No thread %s on %s.", threadID, f.name)
	}
	return thread, nil
}

func (f *fakeApp) LiveState(_ context.Context, threadID string) (LiveState, error) {
	return f.live[threadID], nil
}

func (f *fakeApp) ResolveThreadRef(_ context.Context, ref string) (Resolution, error) {
	if f.failWith != nil {
		return Resolution{}, f.failWith
	}
	if slices.Contains(f.movedRefs, ref) {
		return Resolution{MovedTo: f.movedTo, MovedToName: f.movedToName}, nil
	}
	var matches []Candidate
	ids := make([]string, 0, len(f.threads))
	for id := range f.threads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if strings.HasPrefix(id, ref) {
			matches = append(matches, Candidate{ThreadID: id, Title: f.threads[id].Title})
		}
		if len(matches) == MaxResolutionCandidates {
			break
		}
	}
	return Resolution{Matches: matches}, nil
}

func (f *fakeApp) ResolveWindow(_ context.Context, q WindowQuery) (WindowBounds, error) {
	f.windows = append(f.windows, q)
	items := f.items[q.ThreadID]
	if len(items) == 0 {
		return WindowBounds{Empty: true}, nil
	}
	high := items[len(items)-1].Position
	turns := []string{}
	for _, item := range items {
		if item.TurnID != "" && (len(turns) == 0 || turns[len(turns)-1] != item.TurnID) {
			turns = append(turns, item.TurnID)
		}
	}
	bounds := WindowBounds{From: items[0].Position, To: high, HighWater: high}
	switch q.Kind {
	case WindowAll:
	case WindowTail:
		if q.Turns < len(turns) {
			bounds.From = firstPositionOfTurn(items, turns[len(turns)-q.Turns])
		}
	case WindowHead:
		if q.Turns < len(turns) {
			bounds.To = lastPositionOfTurn(items, turns[q.Turns-1])
		}
	case WindowSince:
		if q.ItemID != "" {
			bounds.From = positionOf(items, q.ItemID) + 1
		}
	case WindowAround:
		index := slices.Index(turns, turnOf(items, q.ItemID))
		low := max(0, index-q.Turns)
		hi := min(len(turns)-1, index+q.Turns)
		bounds.From = firstPositionOfTurn(items, turns[low])
		bounds.To = lastPositionOfTurn(items, turns[hi])
	}
	return bounds, nil
}

func firstPositionOfTurn(items []Item, turn string) int64 {
	for _, item := range items {
		if item.TurnID == turn {
			return item.Position
		}
	}
	return items[0].Position
}

func lastPositionOfTurn(items []Item, turn string) int64 {
	position := items[len(items)-1].Position
	for _, item := range items {
		if item.TurnID == turn {
			position = item.Position
		}
	}
	return position
}

func positionOf(items []Item, itemID string) int64 {
	for _, item := range items {
		if item.ID == itemID {
			return item.Position
		}
	}
	return 0
}

func turnOf(items []Item, itemID string) string {
	for _, item := range items {
		if item.ID == itemID {
			return item.TurnID
		}
	}
	return ""
}

func (f *fakeApp) Transcript(_ context.Context, q TranscriptQuery) (TranscriptSlice, error) {
	f.slices = append(f.slices, q)
	if f.onTranscript != nil {
		return f.onTranscript(q)
	}
	items := f.items[q.ThreadID]
	out := make([]Item, 0, q.Limit)
	for _, item := range items {
		if item.Position < q.From || item.Position > q.To {
			continue
		}
		copied := item
		if !includes(q.Include, copied.Kind) {
			copied.Text = ""
		}
		if q.MaxItemBytes > 0 && len(copied.Text) > q.MaxItemBytes {
			copied.Text = copied.Text[:q.MaxItemBytes]
			copied.Clipped = true
		}
		out = append(out, copied)
		if len(out) == q.Limit {
			break
		}
	}
	high := int64(0)
	if len(items) > 0 {
		high = items[len(items)-1].Position
	}
	return TranscriptSlice{Items: out, HighWater: high}, nil
}

// includes mirrors the App contract: prose rows always carry their body,
// everything else only when the include list asks for it.
func includes(include []string, kind string) bool {
	switch kind {
	case "user_text", "assistant_text", "error":
		return true
	}
	if slices.Contains(include, IncludeAll) {
		return true
	}
	switch kind {
	case "thinking":
		return slices.Contains(include, IncludeThinking)
	case "tool_output":
		return slices.Contains(include, IncludeToolOutputs)
	case "diff":
		return slices.Contains(include, IncludeDiffs)
	case "subagent":
		return slices.Contains(include, IncludeSubagents)
	}
	return false
}

func (f *fakeApp) ItemPayload(_ context.Context, q PayloadQuery) (Payload, error) {
	f.reads = append(f.reads, q)
	body, ok := f.payloads[q.ThreadID+"/"+q.ItemID]
	if !ok {
		return Payload{}, publicf(CodeNotFound, "No item %s in thread %s.", q.ItemID, q.ThreadID)
	}
	payload := Payload{Kind: f.kinds[q.ThreadID+"/"+q.ItemID], Size: int64(len(body)), Offset: q.Offset}
	if q.MaxBytes == 0 || q.Offset >= int64(len(body)) {
		return payload, nil
	}
	end := q.Offset + q.MaxBytes
	if end > int64(len(body)) {
		end = int64(len(body))
	}
	payload.Bytes = body[q.Offset:end]
	return payload, nil
}

func (f *fakeApp) SearchThreads(_ context.Context, q SearchQuery) (SearchPage, error) {
	f.searches = append(f.searches, q)
	if f.onSearch != nil {
		return f.onSearch(q)
	}
	rows := f.hits
	if q.Offset > 0 {
		if q.Offset >= len(rows) {
			rows = nil
		} else {
			rows = rows[q.Offset:]
		}
	}
	more := false
	if q.Limit > 0 && len(rows) > q.Limit {
		rows, more = rows[:q.Limit], true
	}
	return SearchPage{Rows: rows, Indexing: f.indexing, More: more}, nil
}

func (f *fakeApp) ExportTranscript(_ context.Context, _ ExportQuery) (ExportFile, error) {
	return f.export, nil
}

func (f *fakeApp) ExportAnswer(_ context.Context, _ Caller, _ string) (ExportFile, error) {
	return f.export, nil
}

func (f *fakeApp) Catalog(_ context.Context, _ CatalogQuery) (Catalog, error) {
	return f.catalog, nil
}

func (f *fakeApp) Spawn(_ context.Context, _ Caller, call SpawnCall) (RequestAck, error) {
	f.spawns = append(f.spawns, call)
	return f.ack, nil
}

func (f *fakeApp) Send(_ context.Context, _ Caller, call SendCall) (RequestAck, error) {
	f.sends = append(f.sends, call)
	return f.ack, nil
}

func (f *fakeApp) Ask(_ context.Context, _ Caller, call AskCall) (RequestAck, error) {
	f.asks = append(f.asks, call)
	return f.ack, nil
}

func (f *fakeApp) Reply(_ context.Context, _ Caller, call ReplyCall) (ReplyAck, error) {
	f.replies = append(f.replies, call)
	return f.replyAck, nil
}

func (f *fakeApp) RequestStates(_ context.Context, _ Caller, call StatusCall) (StatusReport, error) {
	f.states = append(f.states, call)
	return f.status, nil
}

func (f *fakeApp) ListRequests(_ context.Context, _ Caller, call ListCall) (RequestListing, error) {
	f.lists = append(f.lists, call)
	return f.listing, nil
}

func (f *fakeApp) Cancel(_ context.Context, _ Caller, call CancelCall) (CancelReport, error) {
	f.cancels = append(f.cancels, call)
	return f.cancelled, nil
}

func (f *fakeApp) Remind(_ context.Context, _ Caller, call RemindCall) (RequestAck, error) {
	f.rem = append(f.rem, call)
	return f.ack, nil
}

func (f *fakeApp) UpdateThreads(_ context.Context, _ Caller, call UpdateCall) (UpdateReport, error) {
	f.updates = append(f.updates, call)
	if f.updated != nil {
		return UpdateReport{Results: f.updated}, nil
	}
	results := make([]ThreadUpdateResult, 0, len(call.ThreadIDs))
	for _, id := range call.ThreadIDs {
		results = append(results, ThreadUpdateResult{ThreadID: id, Updated: true})
	}
	return UpdateReport{Results: results}, nil
}

func (f *fakeApp) UpdateGroup(_ context.Context, _ Caller, call GroupCall) (GroupReport, error) {
	f.gcalls = append(f.gcalls, call)
	return f.groupRes, nil
}

func (f *fakeApp) PairedComputers(context.Context) ([]Computer, error) { return f.computers, nil }

func (f *fakeApp) Peer(_ context.Context, computerID string) (Peer, error) {
	if err := f.peerErr[computerID]; err != nil {
		return nil, err
	}
	peer, ok := f.peers[computerID]
	if !ok {
		return nil, publicf(CodeUnreachable, "%s is not reachable from %s.", computerID, f.name)
	}
	return peer, nil
}

// serverPeer routes a peer call into another computer's own Server, so a
// cross-computer test exercises the real destination handler.
type serverPeer struct {
	computer Computer
	server   *Server
	caller   Caller
	queries  []string
	invokes  []string
	fetched  []string
	resolves int
}

func (p *serverPeer) Computer() Computer { return p.computer }

func (p *serverPeer) Resolve(ctx context.Context, ref string) (Resolution, error) {
	p.resolves++
	return p.server.app.ResolveThreadRef(ctx, ref)
}

func (p *serverPeer) Query(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	p.queries = append(p.queries, name)
	return p.run(ctx, name, args)
}

func (p *serverPeer) Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	p.invokes = append(p.invokes, name)
	return p.run(ctx, name, args)
}

// FetchExport stands in for the copy a real peer client makes: the
// destination's export id becomes a path on the calling computer.
func (p *serverPeer) FetchExport(_ context.Context, file ExportFile) (ExportFile, error) {
	p.fetched = append(p.fetched, file.ExportID)
	return ExportFile{Path: "/local/exports/" + file.ExportID, Size: file.Size, SHA256: file.SHA256}, nil
}

func (p *serverPeer) run(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	result, err := p.server.Call(ctx, p.caller, name, args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// brokenPeer is a computer that answers nothing.
type brokenPeer struct {
	computer Computer
	err      error
}

func (p brokenPeer) Computer() Computer { return p.computer }
func (p brokenPeer) Resolve(context.Context, string) (Resolution, error) {
	return Resolution{}, p.err
}
func (p brokenPeer) Query(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, p.err
}
func (p brokenPeer) Invoke(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, p.err
}
func (p brokenPeer) FetchExport(context.Context, ExportFile) (ExportFile, error) {
	return ExportFile{}, p.err
}

// call runs one tool and returns the result as a generic JSON map, which
// is what the transport actually hands the model.
func call(t *testing.T, server *Server, caller Caller, name string, args string) map[string]any {
	t.Helper()
	result, err := server.Call(context.Background(), caller, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("%s: marshal result: %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: decode result: %v", name, err)
	}
	return out
}

// callErr runs one tool and requires a refusal with the given code.
func callErr(t *testing.T, server *Server, caller Caller, name, args, wantCode string) string {
	t.Helper()
	_, err := server.Call(context.Background(), caller, name, json.RawMessage(args))
	if err == nil {
		t.Fatalf("%s: expected a refusal, got a result", name)
	}
	code, message := publicMessage(err)
	if code != wantCode {
		t.Fatalf("%s: code = %q (%s), want %q", name, code, message, wantCode)
	}
	return message
}

func localCaller() Caller {
	return Caller{ThreadID: "caller-thread", ComputerID: "laptop", ComputerName: "Laptop", Title: "Caller"}
}

func rows(t *testing.T, value any) []any {
	t.Helper()
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("expected a list, got %T (%v)", value, value)
	}
	return list
}

func field(t *testing.T, value any, key string) any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T", value)
	}
	return object[key]
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}
