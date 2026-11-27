package trackertools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/trackerdash"
)

// dashboard_widget: an agent counts a widget on a board of the thread's
// project, as the board's dashboards count it, and with show proposes it
// there for the person to keep or dismiss (trackerapp.Service.TryWidget).
// Called with no widget it answers the rules a widget follows
// (trackerdash.Rules), so only a thread that writes one reads them.
// Proposing changes nothing a person has not kept, so it needs no
// approval and is open to a read-only thread.

// CodeNoBoard is a widget for a thread whose project lists no board, or
// lists several and named none of them.
const CodeNoBoard = "tracker_no_board"

type widgetArgs struct {
	Board  string          `json:"board"`
	Widget json.RawMessage `json:"widget"`
	Show   bool            `json:"show"`
}

// widgetSpec is the widget an agent writes, as the rules name its fields.
type widgetSpec struct {
	Title  string   `json:"title"`
	Words  []string `json:"words"`
	SQL    string   `json:"sql"`
	Reduce string   `json:"reduce"`
	Unit   string   `json:"unit"`
	Good   string   `json:"good"`
	Bucket string   `json:"bucket"`
	Series []string `json:"series"`
	Form   string   `json:"form"`
	Width  int      `json:"width"`
	// Limit and Pin are what Keep gives the widget's tile: a number's
	// limit, and a place on the board's summary row.
	Limit *float64 `json:"limit"`
	Pin   bool     `json:"pin"`
}

// widgetAnswer is a counted widget as the agent reads it: what it counts
// as How this is counted says it, the numbers it draws over the range,
// in their unit (seconds for logged time or a time board's estimates,
// which the screen shows as hours), the tickets each number could not
// count, the days of the person's logged time each could not read (not
// known until the board's computer reads them) or whether it counts days
// older than the logged time kept (never known), why it counts
// nothing when it does not (Note), that what it counts of credit may lack
// what a failed read of who worked on the tickets was to read (Evidence),
// how many rows its query returned, the first of them, and, when shown,
// where the person finds it.
type widgetAnswer struct {
	Board    string   `json:"board"`
	Range    string   `json:"range"`
	Counts   string   `json:"counts"`
	Unit     string   `json:"unit"`
	Value    *float64 `json:"value,omitempty"`
	Previous *float64 `json:"previous,omitempty"`
	// PreviousNotCounted is the range before's tickets not counted.
	PreviousNotCounted int `json:"previous_not_counted,omitempty"`
	// NotRead and PreviousNotRead are the days of logged time not read: a
	// number's, else the widget's (a chart or list of no rows yet). Old
	// and PreviousOld say a number, a list or a ranked chart counts days of
	// logged time older than the 182 days kept, so it is never known.
	NotRead         int             `json:"not_read,omitempty"`
	PreviousNotRead int             `json:"previous_not_read,omitempty"`
	Old             bool            `json:"old,omitempty"`
	PreviousOld     bool            `json:"previous_old,omitempty"`
	Note            string          `json:"note,omitempty"`
	Evidence        string          `json:"evidence,omitempty"`
	Series          []string        `json:"series,omitempty"`
	Buckets         []widgetBucket  `json:"buckets,omitempty"`
	More            int             `json:"more,omitempty"`
	NotCounted      int             `json:"not_counted,omitempty"`
	Rows            int             `json:"rows"`
	First           []widgetRow     `json:"first_rows"`
	Partial         string          `json:"partial,omitempty"`
	Shown           string          `json:"shown,omitempty"`
	List            []widgetListRow `json:"list,omitempty"`
}

type widgetBucket struct {
	Label string     `json:"label"`
	Cells []*float64 `json:"cells,omitempty"`
	// CellsNotCounted is each cell's tickets not counted, when one has any.
	CellsNotCounted []int    `json:"cells_not_counted,omitempty"`
	Total           *float64 `json:"total"`
	NotCounted      int      `json:"not_counted,omitempty"`
	NotRead         int      `json:"not_read,omitempty"`
	Old             bool     `json:"old,omitempty"`
}

type widgetRow struct {
	Key     string   `json:"key"`
	Title   string   `json:"title,omitempty"`
	Value   *float64 `json:"value,omitempty"`
	Period  string   `json:"period,omitempty"`
	Counted bool     `json:"counted"`
	Note    string   `json:"note,omitempty"`
}

type widgetListRow struct {
	Key   string `json:"key"`
	Title string `json:"title,omitempty"`
}

func (s *Server) dashboardWidget(ctx context.Context, caller Caller, raw json.RawMessage) (any, error) {
	var args widgetArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(args.Widget)) == 0 || string(bytes.TrimSpace(args.Widget)) == "null" {
		return trackerdash.Rules(), nil
	}
	var spec widgetSpec
	dec := json.NewDecoder(bytes.NewReader(args.Widget))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, invalidf("widget: %v. Call dashboard_widget with no widget for its fields.", err)
	}
	project, board, err := s.widgetBoard(caller.Thread, args.Board)
	if err != nil {
		return nil, err
	}
	w := trackerapp.KeptWidget{
		Form: trackerdash.Form(spec.Form), Width: spec.Width,
		Metric: trackerdash.Metric{Title: spec.Title, Words: spec.Words, SQL: spec.SQL, Reduce: trackerdash.Reducer(spec.Reduce),
			Unit: trackerdash.Unit(spec.Unit), Good: trackerdash.Good(spec.Good), Bucket: trackerdash.Bucket(spec.Bucket), Series: spec.Series},
	}
	if w.Width == 0 {
		w.Width = 6
		if w.Form == trackerdash.FormNumber {
			w.Width = 3
		}
	}
	tried, err := s.t.TryWidget(ctx, project, board.Connection, board.Board, w, trackerapp.TileMarks{Limit: spec.Limit, Pin: spec.Pin}, caller.Thread, args.Show)
	if err != nil {
		return nil, err
	}
	out := widgetAnswer{Board: board.Name, Range: tried.Range.Label, Counts: tried.View.How.Counts, Unit: tried.Unit, Rows: tried.Count,
		Value: numOf(tried.Value.Value), Previous: numOf(tried.Value.Previous), Series: tried.Value.Series, More: tried.Value.More,
		NotCounted: tried.Value.NotCounted, Note: tried.Value.Note, Evidence: tried.Value.Evidence, First: []widgetRow{}}
	// A number's tickets not counted are its value's; a list's or a
	// chart's are the widget's.
	if v := tried.Value.Value; v != nil {
		if v.NC > 0 {
			out.NotCounted = v.NC
		}
		out.NotRead, out.Old = v.UR, v.Old
	} else {
		out.NotRead, out.Old = len(tried.Value.Unread), tried.Value.Old
	}
	if p := tried.Value.Previous; p != nil {
		out.PreviousNotCounted, out.PreviousNotRead, out.PreviousOld = p.NC, p.UR, p.Old
	}
	for _, b := range tried.Value.Buckets {
		wb := widgetBucket{Label: b.Label, Total: numOf(&b.Total), NotCounted: b.Total.NC, NotRead: b.Total.UR, Old: b.Total.Old}
		if len(b.Cells) > 1 {
			nc := make([]int, len(b.Cells))
			for i := range b.Cells {
				wb.Cells = append(wb.Cells, numOf(&b.Cells[i]))
				nc[i] = b.Cells[i].NC
			}
			if slices.ContainsFunc(nc, func(n int) bool { return n > 0 }) {
				wb.CellsNotCounted = nc
			}
		}
		out.Buckets = append(out.Buckets, wb)
	}
	for _, r := range tried.Value.Tickets {
		out.List = append(out.List, widgetListRow{Key: r.Key, Title: r.Title})
	}
	for _, r := range tried.Rows {
		out.First = append(out.First, widgetRow{Key: r.Key, Title: r.Title, Value: r.Value, Period: r.Period, Counted: r.Counted, Note: r.Note})
	}
	if tried.Reading {
		out.Partial = "The board's first read has not finished: these count the tickets read so far."
	}
	if args.Show {
		out.Shown = "It is on the board's dashboards for the user to keep or dismiss."
	}
	return out, nil
}

// widgetBoard is the thread's project and the board of it named, by
// name, ID, or site/ID: the project's one board when none is named. A
// name or an ID that more than one board answers to is refused, naming
// each by its site/ID, which the agent passes back and the user tells
// apart. A board listed through two connections of its site is two
// accounts' copies of it, refused naming the connections by the labels
// the user gave them.
func (s *Server) widgetBoard(threadID, named string) (string, ListedBoard, error) {
	project, boards, err := s.h.Boards(threadID)
	if err != nil {
		return "", ListedBoard{}, err
	}
	sources, err := s.t.TicketSources()
	if err != nil {
		return "", ListedBoard{}, err
	}
	// site is a board's site, "" when no connection turned on reaches it
	// (its count then refuses on its own); handle is its site/ID.
	site := func(b ListedBoard) string {
		if i := slices.IndexFunc(sources, func(src trackerapp.TicketSource) bool { return src.ID == b.Connection }); i >= 0 {
			return sources[i].Site
		}
		return ""
	}
	handle := func(b ListedBoard) string {
		if site := site(b); site != "" {
			return site + "/" + b.Board
		}
		return b.Board
	}
	var listed []ListedBoard
	var names []string
	for _, b := range boards {
		if b.Thread {
			listed = append(listed, b)
			names = append(names, fmt.Sprintf("%q", b.Name))
		}
	}
	named = strings.TrimSpace(named)
	var match []ListedBoard
	switch {
	case len(listed) == 0:
		return "", ListedBoard{}, errorsx.Public(CodeNoBoard, "This thread's project lists no board on this computer; the user adds one in the project's Issues settings.", nil)
	case named == "" && len(listed) == 1:
		match = listed
	case named == "":
		return "", ListedBoard{}, errorsx.Public(CodeNoBoard, fmt.Sprintf("This thread's project lists %s; give board.", sayList(names, "and")), nil)
	default:
		for _, b := range listed {
			if strings.EqualFold(b.Name, named) || b.Board == named || strings.EqualFold(handle(b), named) {
				match = append(match, b)
			}
		}
	}
	if len(match) == 0 {
		return "", ListedBoard{}, errorsx.Public(CodeNoBoard, fmt.Sprintf("This thread's project lists no board %q; it lists %s.", named, sayList(names, "and")), nil)
	}
	var each []string
	for _, b := range match {
		if h := fmt.Sprintf("%q at %s", b.Name, handle(b)); !slices.Contains(each, h) {
			each = append(each, h)
		}
	}
	if len(each) > 1 {
		return "", ListedBoard{}, errorsx.Public(CodeNoBoard, fmt.Sprintf("Board %q names more than one board of this thread's project: %s; give board as the site/ID of one.", named, sayList(each, "and")), nil)
	}
	if len(match) > 1 {
		var labels []string
		for _, b := range match {
			if b.ConnectionLabel != "" {
				labels = append(labels, fmt.Sprintf("%q", b.ConnectionLabel))
			}
		}
		which := ""
		if len(labels) == len(match) {
			which = " (" + sayList(labels, "and") + ")"
		}
		return "", ListedBoard{}, errorsx.Public(CodeNoBoard, fmt.Sprintf("Board %q is listed through %d connections of %s%s, each one account's copy; the user removes one of them from the project's boards in its Issues settings.",
			cmp.Or(named, handle(match[0])), len(match), site(match[0]), which), nil)
	}
	return project, match[0], nil
}

// numOf is n's value, nil when it has none (not known, or a gap).
func numOf(n *trackerdash.Num) *float64 {
	if n == nil {
		return nil
	}
	return n.V
}
