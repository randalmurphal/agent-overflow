package threadtools

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
)

// thread_show: read one thread's transcript.
//
// The window is resolved to absolute timeline positions once, by the app,
// and the cursor carries those bounds forward, so a head or around window
// keeps its window across pages and a thread that keeps streaming never
// shifts a page: every page after the first is clipped to the high-water
// position the first page saw. 38k-item threads exist, so nothing here
// materializes more than one page.

// transcriptBatch is how many items one store round trip pulls. A page
// stops on its byte budget long before this on ordinary threads; the bound
// exists so a window of short rows cannot pull the whole thread at once.
const transcriptBatch = 200

type showArgs struct {
	ThreadID   string   `json:"thread_id"`
	ComputerID string   `json:"computer_id"`
	Window     string   `json:"window"`
	Turns      int      `json:"turns"`
	Context    *int     `json:"context"`
	ItemID     string   `json:"item_id"`
	Since      string   `json:"since"`
	Include    []string `json:"include"`
	MaxBytes   int      `json:"max_bytes"`
	ToFile     bool     `json:"to_file"`
	Cursor     string   `json:"cursor"`
}

type showResult struct {
	ThreadID   string `json:"thread_id"`
	ComputerID string `json:"computer_id,omitempty"`
	Computer   string `json:"computer,omitempty"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Window     string `json:"window"`
	From       int64  `json:"from_position,omitempty"`
	To         int64  `json:"to_position,omitempty"`
	Items      int    `json:"items"`
	Bytes      int    `json:"bytes"`
	Done       bool   `json:"done"`
	Cursor     string `json:"cursor,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	// File is set by to_file instead of Transcript.
	File *ExportFile `json:"file,omitempty"`
	Note string      `json:"note,omitempty"`
}

func (c *session) show(ctx context.Context, raw json.RawMessage) (any, error) {
	var args showArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	page, err := decodeCursor(args.Cursor, cursorShow)
	if err != nil {
		return nil, err
	}
	if err := checkShowCursorArgs(args, page); err != nil {
		return nil, err
	}
	budget, err := showBudget(args.MaxBytes)
	if err != nil {
		return nil, err
	}
	target, err := c.resolve(ctx, args.ThreadID, trim(args.ComputerID))
	if err != nil {
		return nil, err
	}
	if !target.Local {
		return c.showOnPeer(ctx, target, args)
	}
	return c.showLocal(ctx, target, args, page, budget)
}

// checkShowCursorArgs lets a call repeat the window parameters its cursor
// already carries, which is what a model does when it re-issues the same
// read with the cursor added, and refuses only a parameter that names a
// different read than the one the cursor continues.
func checkShowCursorArgs(args showArgs, page cursor) error {
	if page.Kind != cursorShow {
		return nil
	}
	conflict := func(name string) error {
		return invalidf("%s does not match the read this cursor continues. Pass the cursor with the same parameters, or with max_bytes alone, or omit cursor to start a new read.", name)
	}
	if window := trim(args.Window); window != "" && window != page.Window {
		return conflict("window")
	}
	if args.Turns != 0 && (page.Size == nil || *page.Size != args.Turns || (page.Window != WindowTail && page.Window != WindowHead)) {
		return conflict("turns")
	}
	if args.Context != nil && (page.Size == nil || *page.Size != *args.Context || page.Window != WindowAround) {
		return conflict("context")
	}
	if id := trim(args.ItemID); id != "" && id != page.Anchor {
		return conflict("item_id")
	}
	if since := trim(args.Since); since != "" {
		at, err := parseTimestamp(since, "since")
		if err != nil {
			return err
		}
		if at != page.Since {
			return conflict("since")
		}
	}
	if len(args.Include) > 0 {
		include, err := normalizeInclude(args.Include)
		if err != nil {
			return err
		}
		if !sameSet(include, page.Include) {
			return conflict("include")
		}
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, value := range a {
		if !slices.Contains(b, value) {
			return false
		}
	}
	return true
}

func (c *session) showLocal(ctx context.Context, target Target, args showArgs, page cursor, budget int) (any, error) {
	window, err := c.showWindow(ctx, target.ThreadID, args, page)
	if err != nil {
		return nil, err
	}
	bounds, include := window.bounds, window.include
	thread, err := c.app.Thread(ctx, target.ThreadID)
	if err != nil {
		return nil, err
	}
	live, err := c.app.LiveState(ctx, target.ThreadID)
	if err != nil {
		return nil, err
	}
	id, name := c.stamp(Computer{ID: target.ComputerID, Name: target.Computer})
	result := showResult{
		ThreadID:   thread.ID,
		ComputerID: id,
		Computer:   name,
		Title:      thread.Title,
		State:      State(thread, live),
		Provider:   thread.Provider,
		Model:      thread.Model,
		Window:     window.name,
		From:       bounds.From,
		To:         bounds.To,
		Done:       true,
	}
	if note := partialNote(target.Partial); note != "" {
		result.Note = note
	}
	if window.name == WindowLastAnswer && result.State == StateRunning && !bounds.Empty {
		result.Note = appendNote(result.Note, "The thread is still running, so this may not be its final answer.")
	}
	if args.ToFile {
		file, err := c.app.ExportTranscript(ctx, ExportQuery{ThreadID: thread.ID, Bounds: bounds, Include: include})
		if err != nil {
			return nil, err
		}
		result.File = &file
		result.Note = appendNote(result.Note, "The whole window was written to that path on "+NameOfComputer(Computer{ID: id, Name: name})+". Read it with your own file tools; included items are written whole and every other row is listed with its item id.")
		return result, nil
	}
	if bounds.Empty {
		result.Transcript = ""
		if window.name == WindowLastAnswer {
			result.Note = appendNote(result.Note, "This thread has no assistant message yet.")
		} else {
			result.Note = appendNote(result.Note, "That window holds no items.")
		}
		return result, nil
	}

	rendered, err := c.renderWindow(ctx, thread.ID, bounds, include, page.Position, window.back, budget)
	if err != nil {
		return nil, err
	}
	result.Items, result.Bytes, result.Transcript = rendered.items, len(rendered.text), rendered.text
	result.Done = rendered.done
	if rendered.folded {
		result.Note = appendNote(result.Note, "Lines in parentheses fold a run of tool calls and thinking whose bodies this read leaves out. include tool_calls lists each call with its item id; tool_outputs, thinking or diffs add their bodies.")
	}
	if rendered.listed {
		result.Note = appendNote(result.Note, "Read a row's body with thread_item and its item id.")
	}
	if !rendered.done {
		next := window.cursor
		next.Position = rendered.edge
		encoded, err := encodeCursor(next)
		if err != nil {
			return nil, err
		}
		result.Cursor = encoded
		if window.back {
			result.Note = appendNote(result.Note, "This page holds the newest rows of the window and stopped on its byte budget. Pass cursor back unchanged to read the rows before them, or use to_file for the whole window.")
		} else {
			result.Note = appendNote(result.Note, "This page stopped on its byte budget. Pass cursor back unchanged to continue the same window, or use to_file for the whole of it.")
		}
	}
	return result, nil
}

// showRead is one resolved thread_show read: the window, its absolute
// bounds and include list, which end it fills from, and the cursor that
// continues it, minus the position the page reaches.
type showRead struct {
	name    string
	bounds  WindowBounds
	include []string
	back    bool
	cursor  cursor
}

// showWindow resolves the read, taking it from the cursor when one was
// passed.
func (c *session) showWindow(ctx context.Context, threadID string, args showArgs, page cursor) (showRead, error) {
	if page.Kind == cursorShow {
		if page.Thread != threadID {
			return showRead{}, invalidf("That cursor belongs to another thread. Pass the cursor from this thread's own thread_show result.")
		}
		return showRead{
			name:    page.Window,
			bounds:  WindowBounds{From: page.From, To: page.To, HighWater: page.High},
			include: page.Include,
			back:    page.Back,
			cursor:  page,
		}, nil
	}
	query, err := windowQuery(threadID, args)
	if err != nil {
		return showRead{}, err
	}
	include, err := normalizeInclude(args.Include)
	if err != nil {
		return showRead{}, err
	}
	bounds, err := c.app.ResolveWindow(ctx, query)
	if err != nil {
		return showRead{}, err
	}
	read := showRead{name: query.Kind, bounds: bounds, include: include, back: query.Kind == WindowTail}
	read.cursor = cursor{
		Kind: cursorShow, Thread: threadID, Window: query.Kind,
		From: bounds.From, To: bounds.To, High: bounds.HighWater, Include: include,
		Anchor: query.ItemID, Since: query.SinceUnixMs, Back: read.back,
	}
	switch query.Kind {
	case WindowTail, WindowHead, WindowAround:
		size := query.Turns
		read.cursor.Size = &size
	}
	return read, nil
}

func windowQuery(threadID string, args showArgs) (WindowQuery, error) {
	query := WindowQuery{ThreadID: threadID, Kind: trim(args.Window), ItemID: trim(args.ItemID)}
	if query.Kind == "" {
		query.Kind = WindowTail
	}
	if !slices.Contains([]string{WindowTail, WindowHead, WindowSince, WindowAround, WindowAll, WindowLastAnswer}, query.Kind) {
		return WindowQuery{}, invalidf("window must be one of tail, head, since, around, all or last_answer.")
	}
	if args.Turns < 0 || args.Turns > MaxTurns {
		return WindowQuery{}, invalidf("turns must be between 1 and %d.", MaxTurns)
	}
	if args.Context != nil && (*args.Context < 0 || *args.Context > MaxTurns) {
		return WindowQuery{}, invalidf("context must be between 0 and %d.", MaxTurns)
	}
	switch query.Kind {
	case WindowTail, WindowHead:
		query.Turns = args.Turns
		if query.Turns == 0 {
			query.Turns = DefaultTailTurns
		}
	case WindowAround:
		if query.ItemID == "" {
			return WindowQuery{}, invalidf("window around needs item_id, the item to read around. A thread_search hit carries thread_id and item_id together.")
		}
		query.Turns = 1
		if args.Context != nil {
			query.Turns = *args.Context
		}
	case WindowSince:
		if query.ItemID == "" && trim(args.Since) == "" {
			return WindowQuery{}, invalidf("window since needs item_id or since, the point to read after.")
		}
		if query.ItemID == "" {
			at, err := parseTimestamp(args.Since, "since")
			if err != nil {
				return WindowQuery{}, err
			}
			query.SinceUnixMs = at
		}
	case WindowLastAnswer:
		// One row, found by the App; nothing sizes or anchors it.
		query.ItemID = ""
	}
	return query, nil
}

func normalizeInclude(include []string) ([]string, error) {
	known := []string{IncludeToolCalls, IncludeThinking, IncludeToolOutputs, IncludeDiffs, IncludeSubagents, IncludeAll}
	out := make([]string, 0, len(include))
	for _, kind := range include {
		kind = trim(kind)
		if !slices.Contains(known, kind) {
			return nil, invalidf("include accepts %s.", joinNames(known))
		}
		if kind == IncludeAll {
			return []string{IncludeAll}, nil
		}
		if !slices.Contains(out, kind) {
			out = append(out, kind)
		}
	}
	return out, nil
}

func showBudget(maxBytes int) (int, error) {
	if maxBytes == 0 {
		return DefaultShowBytes, nil
	}
	if maxBytes < MinShowBytes || maxBytes > MaxShowBytes {
		return 0, invalidf("max_bytes must be between %d and %d. For more than that, use to_file.", MinShowBytes, MaxShowBytes)
	}
	return maxBytes, nil
}

// checkComputerArg refuses a computer parameter on a computer that has no
// paired computers, where the schema does not carry one.
func (c *session) checkComputerArg(id string) error {
	if trim(id) == "" || c.paired() {
		return nil
	}
	return invalidf("This computer has no paired computers, so computer_id is not a parameter of these tools. Omit it.")
}

// showOnPeer forwards the whole call to the computer that owns the thread
// and returns its result with this computer's view of it stamped on. The
// destination answers in its own shape and does not know what this
// computer calls it.
func (c *session) showOnPeer(ctx context.Context, target Target, args showArgs) (any, error) {
	peer, err := c.app.Peer(ctx, target.ComputerID)
	if err != nil {
		return nil, err
	}
	args.ThreadID, args.ComputerID = target.ThreadID, ""
	raw, err := forwardArgs(args)
	if err != nil {
		return nil, err
	}
	answer, err := peer.Query(ctx, "thread_show", raw)
	if err != nil {
		return nil, err
	}
	var result showResult
	if err := peerResult(answer, &result); err != nil {
		return nil, err
	}
	result.ComputerID, result.Computer = c.stamp(Computer{ID: target.ComputerID, Name: target.Computer})
	if result.File != nil {
		// The destination wrote the window to its own disk. Copy it here
		// and replace both the path and the note it came with, because
		// neither describes a file this computer's model can open.
		local, err := peer.FetchExport(ctx, *result.File)
		if err != nil {
			return nil, err
		}
		result.File = &local
		result.Note = "The whole window was copied from " + NameOfComputer(Computer{ID: result.ComputerID, Name: result.Computer}) +
			" to that path on this computer. Read it with your own file tools; included items are written whole."
	}
	if note := partialNote(target.Partial); note != "" {
		result.Note = appendNote(result.Note, note)
	}
	return result, nil
}

// forwardArgs re-marshals a decoded argument struct for a peer call. Zero
// fields are dropped so the destination applies its own defaults, except a
// pointer field the caller set, whose zero is a value (context 0 is not the
// default context). The computer selector never travels: the destination
// is already the owner.
func forwardArgs(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	set := setPointerFields(value)
	for key, field := range fields {
		if set[key] {
			continue
		}
		switch typed := field.(type) {
		case string:
			if typed == "" {
				delete(fields, key)
			}
		case float64:
			if typed == 0 {
				delete(fields, key)
			}
		case bool:
			if !typed {
				delete(fields, key)
			}
		case nil:
			delete(fields, key)
		case []any:
			if len(typed) == 0 {
				delete(fields, key)
			}
		}
	}
	delete(fields, "computer_id")
	return json.Marshal(fields)
}

// setPointerFields names the JSON keys of a struct's non-nil pointer fields.
func setPointerFields(value any) map[string]bool {
	set := map[string]bool{}
	v := reflect.Indirect(reflect.ValueOf(value))
	if v.Kind() != reflect.Struct {
		return set
	}
	for index := range v.NumField() {
		field := v.Field(index)
		if field.Kind() != reflect.Pointer || field.IsNil() {
			continue
		}
		name, _, _ := strings.Cut(v.Type().Field(index).Tag.Get("json"), ",")
		set[name] = true
	}
	return set
}
