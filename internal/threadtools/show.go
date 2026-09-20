package threadtools

import (
	"context"
	"encoding/json"
	"fmt"
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
	if args.Cursor != "" && (args.Window != "" || args.Turns != 0 || args.Context != nil || args.ItemID != "" || args.Since != "" || len(args.Include) > 0) {
		return nil, invalidf("cursor already carries the window, its bounds and the include list. Pass cursor alone, with max_bytes if you want a different budget.")
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

func (c *session) showLocal(ctx context.Context, target Target, args showArgs, page cursor, budget int) (any, error) {
	window, bounds, include, err := c.showWindow(ctx, target.ThreadID, args, page)
	if err != nil {
		return nil, err
	}
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
		Window:     window,
		From:       bounds.From,
		To:         bounds.To,
		Done:       true,
	}
	if note := partialNote(target.Partial); note != "" {
		result.Note = note
	}
	if args.ToFile {
		file, err := c.app.ExportTranscript(ctx, ExportQuery{ThreadID: thread.ID, Bounds: bounds, Include: include})
		if err != nil {
			return nil, err
		}
		result.File = &file
		result.Note = appendNote(result.Note, "The whole window was written to that path on "+NameOfComputer(Computer{ID: id, Name: name})+". Read it with your own file tools; included items are written whole.")
		return result, nil
	}
	if bounds.Empty {
		result.Transcript = ""
		result.Note = appendNote(result.Note, "That window holds no items.")
		return result, nil
	}

	rendered, err := c.renderWindow(ctx, thread.ID, bounds, include, page.Position, budget)
	if err != nil {
		return nil, err
	}
	result.Items, result.Bytes, result.Transcript = rendered.items, len(rendered.text), rendered.text
	result.Done = rendered.done
	if !rendered.done {
		encoded, err := encodeCursor(cursor{
			Kind: cursorShow, Thread: thread.ID, Window: window,
			From: bounds.From, To: bounds.To, Position: rendered.last, High: bounds.HighWater, Include: include,
		})
		if err != nil {
			return nil, err
		}
		result.Cursor = encoded
		result.Note = appendNote(result.Note, "This page stopped on its byte budget. Pass cursor back unchanged to continue the same window, or use to_file for the whole of it.")
	}
	return result, nil
}

// showWindow returns the window name, its absolute bounds and the include
// list, taking them from the cursor when one was passed.
func (c *session) showWindow(ctx context.Context, threadID string, args showArgs, page cursor) (string, WindowBounds, []string, error) {
	if page.Kind == cursorShow {
		if page.Thread != threadID {
			return "", WindowBounds{}, nil, invalidf("That cursor belongs to another thread. Pass the cursor from this thread's own thread_show result.")
		}
		return page.Window, WindowBounds{From: page.From, To: page.To, HighWater: page.High}, page.Include, nil
	}
	query, err := windowQuery(threadID, args)
	if err != nil {
		return "", WindowBounds{}, nil, err
	}
	include, err := normalizeInclude(args.Include)
	if err != nil {
		return "", WindowBounds{}, nil, err
	}
	bounds, err := c.app.ResolveWindow(ctx, query)
	if err != nil {
		return "", WindowBounds{}, nil, err
	}
	return query.Kind, bounds, include, nil
}

func windowQuery(threadID string, args showArgs) (WindowQuery, error) {
	query := WindowQuery{ThreadID: threadID, Kind: trim(args.Window), ItemID: trim(args.ItemID)}
	if query.Kind == "" {
		query.Kind = WindowTail
	}
	if !slices.Contains([]string{WindowTail, WindowHead, WindowSince, WindowAround, WindowAll}, query.Kind) {
		return WindowQuery{}, invalidf("window must be one of tail, head, since, around or all.")
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
	}
	return query, nil
}

func normalizeInclude(include []string) ([]string, error) {
	known := []string{IncludeThinking, IncludeToolOutputs, IncludeDiffs, IncludeSubagents, IncludeAll}
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
// fields are dropped so the destination applies its own defaults, and the
// computer selector never travels: the destination is already the owner.
func forwardArgs(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key, field := range fields {
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

type renderedPage struct {
	text  string
	items int
	last  int64
	done  bool
}

// renderWindow walks the window in batches and renders until the budget is
// spent. It never holds more than one batch, and it always makes progress:
// the first row of a page is rendered even when it alone fills the budget,
// clipped with a pointer to thread_item.
func (c *session) renderWindow(ctx context.Context, threadID string, bounds WindowBounds, include []string, after int64, budget int) (renderedPage, error) {
	from := bounds.From
	if after > 0 {
		from = after + 1
	}
	// The bounds are absolute, so they are used as given. The only
	// adjustment is the snapshot clamp, which is what keeps a page from
	// shifting when the thread streams on while it is being read.
	to := bounds.To
	if bounds.HighWater > 0 && to > bounds.HighWater {
		to = bounds.HighWater
	}
	page := renderedPage{last: after, done: true}
	var out strings.Builder
	var turn string
	itemBudget := budget / 4
	if itemBudget < 2048 {
		itemBudget = 2048
	}
	for from <= to {
		slice, err := c.app.Transcript(ctx, TranscriptQuery{
			ThreadID: threadID, From: from, To: to, Limit: transcriptBatch,
			Include: include, MaxItemBytes: itemBudget,
		})
		if err != nil {
			return renderedPage{}, err
		}
		if len(slice.Items) == 0 {
			return finishPage(page, &out), nil
		}
		for _, item := range slice.Items {
			block := renderItem(threadID, item)
			if item.TurnID != "" && item.TurnID != turn {
				turn = item.TurnID
				block = "--- turn " + item.TurnID + " ---\n" + block
			}
			if out.Len() > 0 && out.Len()+len(block)+1 > budget {
				page.done = false
				return finishPage(page, &out), nil
			}
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString(block)
			page.items++
			page.last = item.Position
			from = item.Position + 1
		}
		if len(slice.Items) < transcriptBatch {
			return finishPage(page, &out), nil
		}
	}
	return finishPage(page, &out), nil
}

func finishPage(page renderedPage, out *strings.Builder) renderedPage {
	page.text = out.String()
	return page
}

// renderItem renders one timeline row: role and item id prefix, the body
// when it is there, and a pointer to thread_item when it is not or when it
// was clipped. A tool call with no body collapses to the one line the spec
// asks for, stating its item id and the size it holds.
func renderItem(threadID string, item Item) string {
	role := item.Role
	if role == "" {
		role = item.Kind
	}
	head := fmt.Sprintf("[%s %s]", role, item.ID)
	if item.Name != "" {
		head += " " + item.Name
	}
	pointer := fmt.Sprintf("thread_item thread_id=%s item_id=%s", threadID, item.ID)
	switch {
	case item.Text == "" && item.Size > 0:
		return fmt.Sprintf("%s (%s, not shown; read it with %s)", head, humanBytes(item.Size), pointer)
	case item.Text == "":
		return head
	case item.Clipped:
		return fmt.Sprintf("%s %s\n… clipped at %s of %s; read the rest with %s", head, item.Text, humanBytes(int64(len(item.Text))), humanBytes(item.Size), pointer)
	default:
		return head + " " + item.Text
	}
}
