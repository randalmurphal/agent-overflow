package main

// `bench --trace` records a Chromium timeline trace around the workload
// and answers one question from it: which JS call sites FORCED layout or
// style recalculation.
//
// WHAT MAKES AN EVENT FORCED. Chromium emits `UpdateLayoutTree` and
// `Layout` for both halves of the world — the ones the engine schedules
// for itself at the end of a frame, and the ones a script triggered by
// reading `offsetHeight` (or a hundred other invalidating properties)
// mid-task. Only the second kind carries `args.beginData.stackTrace`,
// because only the second kind HAS a JS stack to attribute: the engine's
// own end-of-frame pass runs from nothing. So the stack is the signal,
// not a heuristic over it, and its top frame is the line to go read.
//
// The parsing half is pure and tested against a canned trace; the session
// half is three protocol calls and a stream read.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-overflow/internal/cdpclient"
)

// traceCategories is the smallest set that carries the two event names
// with their stacks. `disabled-by-default-devtools.timeline.stack` is the
// one that actually attaches `beginData.stackTrace`; without it the trace
// records the same events with nothing to attribute them to, and every
// forced layout reads as an engine-scheduled one.
func traceCategories() []string {
	return []string{
		"devtools.timeline",
		"disabled-by-default-devtools.timeline",
		"disabled-by-default-devtools.timeline.stack",
		"v8.execute",
	}
}

// traceStreamChunk is how much of the trace stream one IO.read asks for.
// A megabyte per round trip keeps a 100 MB trace to a hundred calls
// rather than tens of thousands.
const traceStreamChunk = 1 << 20

// traceTopGroups is how many call sites the report keeps. A bench under
// load produces a long tail of one-off stacks; the answer a caller acts
// on is at the top, and an unbounded list would put a thousand entries in
// a file that doubles as a baseline.
const traceTopGroups = 15

// traceSession is one recording. It exists so the stop path is
// unconditional: a bench that failed mid-workload must still end the
// trace, or the next repeat's `Tracing.start` is refused by a browser
// that is already recording.
type traceSession struct {
	conn    *cdpclient.Conn
	running bool
}

func startTracing(ctx context.Context, conn *cdpclient.Conn) (*traceSession, error) {
	session := &traceSession{conn: conn}
	_, err := conn.Call(ctx, "Tracing.start", map[string]any{
		"transferMode": "ReturnAsStream",
		"streamFormat": "json",
		"traceConfig": map[string]any{
			"recordMode":         "recordAsMuchAsPossible",
			"includedCategories": traceCategories(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("start tracing: %w", err)
	}
	session.running = true
	return session, nil
}

// stop ends the recording and reads the whole trace back. The
// subscription is registered BEFORE `Tracing.end`, because the browser
// can deliver `tracingComplete` inside that round trip.
func (s *traceSession) stop(ctx context.Context) ([]byte, error) {
	if s == nil || !s.running {
		return nil, nil
	}
	s.running = false

	sub := s.conn.Subscribe("Tracing.tracingComplete")
	defer sub.Close()
	if _, err := s.conn.Call(ctx, "Tracing.end", nil); err != nil {
		return nil, fmt.Errorf("end tracing: %w", err)
	}
	event, err := sub.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for the trace to finish: %w", err)
	}
	var complete struct {
		Stream         string `json:"stream"`
		DataLossOccurr bool   `json:"dataLossOccurred"`
	}
	if err := json.Unmarshal(event.Params, &complete); err != nil {
		return nil, fmt.Errorf("decode tracingComplete: %w", err)
	}
	if complete.Stream == "" {
		return nil, fmt.Errorf("the browser completed the trace without a stream handle")
	}
	return readTraceStream(ctx, s.conn, complete.Stream)
}

func readTraceStream(ctx context.Context, conn *cdpclient.Conn, handle string) ([]byte, error) {
	var buf []byte
	for {
		var chunk struct {
			Data          string `json:"data"`
			Base64Encoded bool   `json:"base64Encoded"`
			EOF           bool   `json:"eof"`
		}
		if err := conn.CallInto(ctx, &chunk, "IO.read",
			map[string]any{"handle": handle, "size": traceStreamChunk}); err != nil {
			return nil, fmt.Errorf("read the trace stream: %w", err)
		}
		if chunk.Base64Encoded {
			decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
			if err != nil {
				return nil, fmt.Errorf("decode a trace chunk: %w", err)
			}
			buf = append(buf, decoded...)
		} else {
			buf = append(buf, chunk.Data...)
		}
		if chunk.EOF {
			break
		}
	}
	// Best effort: the handle is freed when the browser drops the trace
	// anyway, and a failure here must not lose a trace already in hand.
	_, _ = conn.Call(ctx, "IO.close", map[string]any{"handle": handle})
	return buf, nil
}

// readTraceSummary ends a recording (if there is one) and folds it. A nil
// session answers nothing at all, which is what a bench without --trace
// wants: one call site, no conditional around it.
func readTraceSummary(ctx context.Context, session *traceSession) (*traceSummary, error) {
	data, err := session.stop(ctx)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	events, err := parseTraceEvents(data)
	if err != nil {
		return nil, err
	}
	summary := summarizeForcedLayout(events)
	return &summary, nil
}

// traceFrame is one JS stack frame as the timeline spells it.
type traceFrame struct {
	FunctionName string `json:"functionName"`
	URL          string `json:"url"`
	LineNumber   int    `json:"lineNumber"`
}

// traceEvent is the subset of a trace event this parse reads. `args` is
// raw because the field is a different shape on every event name, and a
// typed decode of one of them would fail the whole document on another.
type traceEvent struct {
	Name string          `json:"name"`
	Cat  string          `json:"cat"`
	Dur  float64         `json:"dur"`
	Args json.RawMessage `json:"args"`
}

// forcedLayoutGroup is one call site's tally.
type forcedLayoutGroup struct {
	// Frame is functionName@url:line — the thing to go open.
	Frame string `json:"frame"`
	Count int    `json:"count"`
	// Style and Layout split the count by which pass was forced:
	// UpdateLayoutTree is a forced style recalculation, Layout is a forced
	// reflow, and the fix for the two is often different.
	Style  int     `json:"style"`
	Layout int     `json:"layout"`
	Ms     float64 `json:"ms"`
}

// traceSummary is the whole answer a trace contributes to a bench report.
type traceSummary struct {
	Events       int     `json:"events"`
	ForcedEvents int     `json:"forcedEvents"`
	ForcedMs     float64 `json:"forcedMs"`
	// ScheduledEvents counts the layout/style events with NO stack — the
	// engine's own end-of-frame passes. Reported so a reader can see the
	// ratio rather than assuming every reflow in the run was forced.
	ScheduledEvents int                 `json:"scheduledEvents"`
	Groups          []forcedLayoutGroup `json:"groups,omitempty"`
	// Truncated says how many call sites are NOT in Groups.
	Truncated int `json:"truncated,omitempty"`
}

// parseTraceEvents accepts both shapes the stream arrives in: a bare
// array of events, or the `{"traceEvents": [...]}` wrapper. Chromium has
// emitted each at different times and the difference is invisible until a
// parse silently finds zero events.
func parseTraceEvents(data []byte) ([]traceEvent, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("the trace is empty")
	}
	if strings.HasPrefix(trimmed, "[") {
		var events []traceEvent
		if err := json.Unmarshal([]byte(trimmed), &events); err != nil {
			return nil, fmt.Errorf("decode trace events: %w", err)
		}
		return events, nil
	}
	var wrapper struct {
		TraceEvents []traceEvent `json:"traceEvents"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wrapper); err != nil {
		return nil, fmt.Errorf("decode trace document: %w", err)
	}
	return wrapper.TraceEvents, nil
}

// summarizeForcedLayout groups the forced layout/style events by their
// top stack frame. Every group is kept here; the caller decides how many
// to print or persist, so a merge across repeats is not done over an
// already-truncated tail.
func summarizeForcedLayout(events []traceEvent) traceSummary {
	summary := traceSummary{Events: len(events)}
	byFrame := map[string]*forcedLayoutGroup{}
	for _, event := range events {
		isStyle := event.Name == "UpdateLayoutTree"
		isLayout := event.Name == "Layout"
		if !isStyle && !isLayout {
			continue
		}
		frame, ok := topStackFrame(event.Args)
		if !ok {
			summary.ScheduledEvents++
			continue
		}
		key := formatTraceFrame(frame)
		group := byFrame[key]
		if group == nil {
			group = &forcedLayoutGroup{Frame: key}
			byFrame[key] = group
		}
		group.Count++
		if isStyle {
			group.Style++
		} else {
			group.Layout++
		}
		group.Ms += event.Dur / 1000
		summary.ForcedEvents++
		summary.ForcedMs += event.Dur / 1000
	}
	summary.Groups = sortTraceGroups(byFrame)
	return summary
}

func sortTraceGroups(byFrame map[string]*forcedLayoutGroup) []forcedLayoutGroup {
	groups := make([]forcedLayoutGroup, 0, len(byFrame))
	for _, group := range byFrame {
		groups = append(groups, *group)
	}
	// Count first: this instrument answers "how many forced layouts", and
	// a call site firing a thousand cheap reflows is the finding even when
	// a rarer one spent more milliseconds. Duration breaks the tie, and the
	// frame name breaks that, so the order is stable across runs.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		if groups[i].Ms != groups[j].Ms {
			return groups[i].Ms > groups[j].Ms
		}
		return groups[i].Frame < groups[j].Frame
	})
	return groups
}

// topStackFrame reads args.beginData.stackTrace[0]. Absent, malformed, or
// empty all answer "no stack", which is the same finding: this event was
// not forced from script.
func topStackFrame(args json.RawMessage) (traceFrame, bool) {
	if len(args) == 0 {
		return traceFrame{}, false
	}
	var decoded struct {
		BeginData struct {
			StackTrace []traceFrame `json:"stackTrace"`
		} `json:"beginData"`
	}
	if err := json.Unmarshal(args, &decoded); err != nil {
		return traceFrame{}, false
	}
	if len(decoded.BeginData.StackTrace) == 0 {
		return traceFrame{}, false
	}
	return decoded.BeginData.StackTrace[0], true
}

func formatTraceFrame(frame traceFrame) string {
	name := frame.FunctionName
	if strings.TrimSpace(name) == "" {
		name = "(anonymous)"
	}
	if frame.URL == "" {
		return name
	}
	return fmt.Sprintf("%s@%s:%d", name, frame.URL, frame.LineNumber)
}

// mergeTraceSummaries folds a bench's repeats into one answer, then keeps
// the top call sites.
func mergeTraceSummaries(summaries []traceSummary) *traceSummary {
	if len(summaries) == 0 {
		return nil
	}
	merged := traceSummary{}
	byFrame := map[string]*forcedLayoutGroup{}
	for _, summary := range summaries {
		merged.Events += summary.Events
		merged.ForcedEvents += summary.ForcedEvents
		merged.ForcedMs += summary.ForcedMs
		merged.ScheduledEvents += summary.ScheduledEvents
		for _, group := range summary.Groups {
			existing := byFrame[group.Frame]
			if existing == nil {
				copied := group
				byFrame[group.Frame] = &copied
				continue
			}
			existing.Count += group.Count
			existing.Style += group.Style
			existing.Layout += group.Layout
			existing.Ms += group.Ms
		}
	}
	groups := sortTraceGroups(byFrame)
	if len(groups) > traceTopGroups {
		merged.Truncated = len(groups) - traceTopGroups
		groups = groups[:traceTopGroups]
	}
	merged.Groups = groups
	return &merged
}

func renderTraceSummary(summary traceSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "forced layout/style: %d event(s) over %.1fms from %d call site(s)"+
		"  (%d engine-scheduled, %d trace events)\n",
		summary.ForcedEvents, summary.ForcedMs, len(summary.Groups)+summary.Truncated,
		summary.ScheduledEvents, summary.Events)
	if len(summary.Groups) == 0 {
		b.WriteString("  no forced layout attributed to a JS stack\n")
		return b.String()
	}
	rows := make([][]string, 0, len(summary.Groups))
	for _, group := range summary.Groups {
		rows = append(rows, []string{
			"  " + truncate(group.Frame, 96),
			fmt.Sprint(group.Count),
			fmt.Sprint(group.Style),
			fmt.Sprint(group.Layout),
			fmt.Sprintf("%.1f", group.Ms),
		})
	}
	b.WriteString(tableString([]string{"  CALL SITE", "FORCED", "STYLE", "LAYOUT", "MS"}, rows))
	if summary.Truncated > 0 {
		fmt.Fprintf(&b, "  %d further call site(s) not shown\n", summary.Truncated)
	}
	return b.String()
}
