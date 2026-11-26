package trackertools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
}

// widgetAnswer is a counted widget as the agent reads it: what it counts
// as How this is counted says it, the numbers it draws over the range,
// how many rows its query returned, the first of them, and, when shown,
// where the person finds it.
type widgetAnswer struct {
	Board      string          `json:"board"`
	Range      string          `json:"range"`
	Counts     string          `json:"counts"`
	Value      *float64        `json:"value,omitempty"`
	Previous   *float64        `json:"previous,omitempty"`
	Series     []string        `json:"series,omitempty"`
	Buckets    []widgetBucket  `json:"buckets,omitempty"`
	More       int             `json:"more,omitempty"`
	NotCounted int             `json:"not_counted,omitempty"`
	Rows       int             `json:"rows"`
	First      []widgetRow     `json:"first_rows"`
	Partial    string          `json:"partial,omitempty"`
	Shown      string          `json:"shown,omitempty"`
	List       []widgetListRow `json:"list,omitempty"`
}

type widgetBucket struct {
	Label string     `json:"label"`
	Cells []*float64 `json:"cells,omitempty"`
	Total *float64   `json:"total"`
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
	tried, err := s.t.TryWidget(ctx, project, board.Connection, board.Board, w, caller.Thread, args.Show)
	if err != nil {
		return nil, err
	}
	out := widgetAnswer{Board: board.Name, Range: tried.Range.Label, Counts: tried.View.How.Counts, Rows: tried.Count,
		Value: numOf(tried.Value.Value), Previous: numOf(tried.Value.Previous), Series: tried.Value.Series, More: tried.Value.More,
		NotCounted: tried.Value.NotCounted, First: []widgetRow{}}
	for _, b := range tried.Value.Buckets {
		wb := widgetBucket{Label: b.Label, Total: numOf(&b.Total)}
		if len(b.Cells) > 1 {
			for i := range b.Cells {
				wb.Cells = append(wb.Cells, numOf(&b.Cells[i]))
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

// widgetBoard is the thread's project and the board of it named, by name
// or ID: the project's one board when none is named.
func (s *Server) widgetBoard(threadID, named string) (string, ListedBoard, error) {
	project, boards, err := s.h.Boards(threadID)
	if err != nil {
		return "", ListedBoard{}, err
	}
	var listed []ListedBoard
	for _, b := range boards {
		if b.Thread {
			listed = append(listed, b)
		}
	}
	named = strings.TrimSpace(named)
	var names []string
	for _, b := range listed {
		if named != "" && (strings.EqualFold(b.Name, named) || b.Board == named) {
			return project, b, nil
		}
		names = append(names, fmt.Sprintf("%q", b.Name))
	}
	switch {
	case len(listed) == 0:
		return "", ListedBoard{}, errorsx.Public(CodeNoBoard, "This thread's project lists no board on this computer; the user adds one in the project's Issues settings.", nil)
	case named == "" && len(listed) == 1:
		return project, listed[0], nil
	case named == "":
		return "", ListedBoard{}, errorsx.Public(CodeNoBoard, fmt.Sprintf("This thread's project lists %s; give board.", sayList(names, "and")), nil)
	}
	return "", ListedBoard{}, errorsx.Public(CodeNoBoard, fmt.Sprintf("This thread's project lists no board %q; it lists %s.", named, sayList(names, "and")), nil)
}

// numOf is n's value, nil when it has none (not known, or a gap).
func numOf(n *trackerdash.Num) *float64 {
	if n == nil {
		return nil
	}
	return n.V
}
