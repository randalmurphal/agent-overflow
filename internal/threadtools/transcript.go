package threadtools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// The thread_show renderer.
//
// A page is built from entries: a row the read shows renders on its own,
// and consecutive rows of one turn whose bodies the read leaves out fold
// into one parenthesized line. A turn of an agent at work is mostly tool
// calls, and a line per call would spend the page on rows that say nothing
// but their size.
//
// A head, since, around, all or last_answer page fills from the start of
// its range. A tail page fills from the end and drops the oldest rows
// first, so the newest turn is always on it; its cursor pages backwards.
// Either way the first entry of a page is rendered even when it alone
// fills the budget, clipped with a pointer to thread_item, so a page
// always makes progress.

type renderedPage struct {
	text  string
	items int
	// edge is the position the next page continues from: the newest row
	// rendered on a forward page, the oldest on a backward one.
	edge int64
	done bool
	// folded and listed say whether the page holds a folded run or a
	// bodiless tool line, so the result explains them once.
	folded bool
	listed bool
}

// entry is one rendered line group and the positions it covers.
type entry struct {
	turn   string
	text   string
	oldest int64
	newest int64
	items  int
	folded bool
	listed bool
}

// renderWindow walks the window in batches and renders until the budget is
// spent, never holding more than one batch. edge continues a previous page:
// the last row it rendered, the first when back is set.
func (c *session) renderWindow(ctx context.Context, threadID string, bounds WindowBounds, include []string, edge int64, back bool, budget int) (renderedPage, error) {
	from, to := bounds.From, bounds.To
	// The bounds are absolute, so they are used as given. The only
	// adjustment is the snapshot clamp, which is what keeps a page from
	// shifting when the thread streams on while it is being read.
	if bounds.HighWater > 0 && to > bounds.HighWater {
		to = bounds.HighWater
	}
	if edge > 0 {
		if back {
			to = edge - 1
		} else {
			from = edge + 1
		}
	}
	itemBudget := max(budget/4, 2048)
	query := TranscriptQuery{
		ThreadID: threadID, Limit: transcriptBatch, Newest: back, Include: include,
		MaxItemBytes: itemBudget, MaxProseBytes: max(budget-proseHeadroom, itemBudget),
	}
	page := newPageBuilder(budget, back)
	fold := folder{threadID: threadID, include: include}
	for from <= to {
		query.From, query.To = from, to
		slice, err := c.app.Transcript(ctx, query)
		if err != nil {
			return renderedPage{}, err
		}
		rows := slice.Items
		for index := range rows {
			item := rows[index]
			if back {
				item = rows[len(rows)-1-index]
			}
			for _, e := range fold.push(item) {
				if !page.add(e) {
					return page.finish(false, edge), nil
				}
			}
			if back {
				to = item.Position - 1
			} else {
				from = item.Position + 1
			}
		}
		if len(rows) < transcriptBatch {
			break
		}
	}
	if e, ok := fold.flush(); ok && !page.add(e) {
		return page.finish(false, edge), nil
	}
	return page.finish(true, edge), nil
}

// pageBuilder budgets entries in walk order. A forward page writes them as
// they come; a backward page keeps them, newest first, and writes them in
// timeline order when it is finished. Both count the same bytes: one turn
// header per contiguous turn on the page and a newline between entries.
type pageBuilder struct {
	budget int
	back   bool
	size   int
	turn   string
	out    strings.Builder
	kept   []entry
	page   renderedPage
	any    bool
}

func newPageBuilder(budget int, back bool) *pageBuilder {
	return &pageBuilder{budget: budget, back: back}
}

func turnHeader(turn string) string { return "--- turn " + turn + " ---\n" }

// add budgets one entry and reports whether it fit. The first entry of a
// page always fits.
func (p *pageBuilder) add(e entry) bool {
	header := e.turn != "" && e.turn != p.turn
	cost := len(e.text)
	if header {
		cost += len(turnHeader(e.turn))
	}
	if p.any {
		cost++
	}
	if p.any && p.size+cost > p.budget {
		return false
	}
	p.size += cost
	if header {
		p.turn = e.turn
	}
	p.page.items += e.items
	p.page.folded = p.page.folded || e.folded
	p.page.listed = p.page.listed || e.listed
	if p.back {
		// finish writes the headers once the page's order is known.
		p.page.edge = e.oldest
		p.kept = append(p.kept, e)
	} else {
		if header {
			e.text = turnHeader(e.turn) + e.text
		}
		p.page.edge = e.newest
		p.write(e)
	}
	p.any = true
	return true
}

func (p *pageBuilder) write(e entry) {
	if p.out.Len() > 0 {
		p.out.WriteString("\n")
	}
	p.out.WriteString(e.text)
}

// finish renders the page. A page that rendered nothing keeps the edge it
// started from.
func (p *pageBuilder) finish(done bool, edge int64) renderedPage {
	if p.back {
		turn := ""
		for index := len(p.kept) - 1; index >= 0; index-- {
			e := p.kept[index]
			if e.turn != "" && e.turn != turn {
				turn = e.turn
				e.text = turnHeader(turn) + e.text
			}
			p.write(e)
		}
	}
	page := p.page
	if !p.any {
		page.edge = edge
	}
	page.text = p.out.String()
	page.done = done
	return page
}

// folder turns rows, in walk order, into entries.
type folder struct {
	threadID string
	include  []string
	run      *foldedRun
}

// push takes the next row and returns the entries it completes: the run
// it ends, the row itself, or nothing while a run is still open.
func (f *folder) push(item Item) []entry {
	var out []entry
	if folds(f.include, item.Kind) {
		if f.run != nil && f.run.turn != item.TurnID {
			out = append(out, f.run.entry())
			f.run = nil
		}
		if f.run == nil {
			f.run = newFoldedRun(item.TurnID)
		}
		f.run.add(item)
		return out
	}
	if f.run != nil {
		out = append(out, f.run.entry())
		f.run = nil
	}
	return append(out, rowEntry(f.threadID, item))
}

// flush closes the run the walk ended inside.
func (f *folder) flush() (entry, bool) {
	if f.run == nil {
		return entry{}, false
	}
	e := f.run.entry()
	f.run = nil
	return e, true
}

// folds reports whether a row of kind folds into a run: its body is left
// out, and it is not a tool row that include tool_calls lists.
func folds(include []string, kind string) bool {
	if IncludesBody(include, kind) {
		return false
	}
	if isToolRow(kind) && slices.Contains(include, IncludeToolCalls) {
		return false
	}
	return true
}

func isToolRow(kind string) bool { return kind == "tool_call" || kind == "diff" }

func rowEntry(threadID string, item Item) entry {
	return entry{
		turn:   item.TurnID,
		text:   renderItem(threadID, item),
		oldest: item.Position,
		newest: item.Position,
		items:  1,
		listed: item.Text == "" && item.Size > 0,
	}
}

// foldedRun counts the rows of one run.
type foldedRun struct {
	turn           string
	oldest, newest int64
	items          int
	calls          int
	thinking       int
	other          int
	bytes          int64
	names          map[string]int
}

func newFoldedRun(turn string) *foldedRun {
	return &foldedRun{turn: turn, names: map[string]int{}}
}

func (r *foldedRun) add(item Item) {
	if r.items == 0 || item.Position < r.oldest {
		r.oldest = item.Position
	}
	if item.Position > r.newest {
		r.newest = item.Position
	}
	r.items++
	r.bytes += item.Size
	switch {
	case isToolRow(item.Kind):
		r.calls++
		name := item.Name
		if name == "" {
			name = "tool"
		}
		r.names[name]++
	case item.Kind == "thinking":
		r.thinking++
	default:
		r.other++
	}
}

// entry renders the run as one line, for example
// "(12 tool calls: Bash 8, Read 3, Edit 1; 2 thinking; 380.0 KB not shown)".
// Names are ordered by count, then name, so the line reads the same
// whichever direction the walk met the rows in.
func (r *foldedRun) entry() entry {
	var parts []string
	if r.calls > 0 {
		part := plural(r.calls, "tool call", "tool calls")
		names := make([]string, 0, len(r.names))
		for name := range r.names {
			names = append(names, name)
		}
		slices.SortFunc(names, func(a, b string) int {
			if r.names[a] != r.names[b] {
				return r.names[b] - r.names[a]
			}
			return strings.Compare(a, b)
		})
		counted := make([]string, 0, min(len(names), maxRunNames)+1)
		for index, name := range names {
			if index == maxRunNames {
				counted = append(counted, "…")
				break
			}
			counted = append(counted, fmt.Sprintf("%s %d", name, r.names[name]))
		}
		parts = append(parts, part+": "+strings.Join(counted, ", "))
	}
	if r.thinking > 0 {
		parts = append(parts, fmt.Sprintf("%d thinking", r.thinking))
	}
	if r.other > 0 {
		parts = append(parts, plural(r.other, "other row", "other rows"))
	}
	if r.bytes > 0 {
		parts = append(parts, humanBytes(r.bytes)+" not shown")
	}
	return entry{
		turn:   r.turn,
		text:   "(" + strings.Join(parts, "; ") + ")",
		oldest: r.oldest,
		newest: r.newest,
		items:  r.items,
		folded: true,
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// RowHead is how a transcript and an export name one row: role, item id,
// and a tool row's name and summary.
func RowHead(item Item) string {
	role := item.Role
	if role == "" {
		role = item.Kind
	}
	head := "[" + role + " " + item.ID + "]"
	if item.Name != "" {
		head += " " + item.Name
	}
	// A summary that only repeats the tool name says nothing more.
	if label := oneLine(item.Label, maxLabelBytes); label != "" && label != item.Name {
		if item.Name != "" {
			head += ":"
		}
		head += " " + label
	}
	return head
}

// oneLine collapses whitespace runs to single spaces and clips to limit
// bytes on a character boundary.
func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// renderItem renders one row: its head, then its body. Prose follows the
// head on its line; a tool body starts on the next line because it is
// usually output. A row with no body states its size, and a clipped one
// says where the rest is.
func renderItem(threadID string, item Item) string {
	head := RowHead(item)
	separator := " "
	if !IsProse(item.Kind) && item.Label != "" {
		separator = "\n"
	}
	switch {
	case item.Text == "" && item.Size > 0:
		return head + " (" + humanBytes(item.Size) + ")"
	case item.Text == "":
		return head
	case item.Clipped:
		return fmt.Sprintf("%s%s%s\n… clipped at %s of %s; read the rest with thread_item thread_id=%s item_id=%s",
			head, separator, item.Text, humanBytes(int64(len(item.Text))), humanBytes(item.Size), threadID, item.ID)
	default:
		return head + separator + item.Text
	}
}
