package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/harnessclient"
)

// uiSnapshotDirName holds the last snapshot this CLI took, per instance:
// the file lives under the instance's own data dir, so two harnesses in
// two worktrees keep separate diff baselines with no extra bookkeeping.
const (
	uiSnapshotDirName  = "ui-snapshots"
	uiSnapshotFileName = "last.json"
)

// bridgeProbeTimeout bounds one "is a page attached" question. A page
// that has not answered in five seconds is not mid-render; it is gone.
const bridgeProbeTimeout = 5 * time.Second

var uiSubcommands = []string{"snapshot", "query", "state", "diff", "reload", "open"}

func runUI(e *env, args []string) error {
	if done, err := groupHelp(e, "ui", args, uiSubcommands...); done {
		return err
	}
	if len(args) == 0 {
		return usagef("ui needs a subcommand: %s", strings.Join(uiSubcommands, ", "))
	}
	switch args[0] {
	case "snapshot":
		return uiSnapshot(e, args[1:])
	case "query":
		return uiQuery(e, args[1:])
	case "state":
		return uiState(e, args[1:])
	case "diff":
		return uiDiffCommand(e, args[1:])
	case "reload":
		return uiReload(e, args[1:])
	case "open":
		return uiOpen(e, args[1:])
	default:
		return usagef("unknown ui subcommand %q (want %s)", args[0], strings.Join(uiSubcommands, ", "))
	}
}

// uiReload and uiOpen expose two things the bench drivers have been
// doing privately since W5. Both are ordinary operator moves — "the page
// is stale after a reset", "put this thread on screen so I can look at
// it" — and neither had a spelling outside a bench run.

func uiReload(e *env, args []string) error {
	flags := e.newFlagSet("ui reload")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("ui reload takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		if err := reloadPage(ctx, e, client); err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"reloaded": true})
		}
		e.printf("page reloaded\n")
		return nil
	})
}

func uiOpen(e *env, args []string) error {
	flags := e.newFlagSet("ui open --thread <id|#N|last|title-prefix>")
	thread := flags.String("thread", "", "thread selector: id, #N from `threads`, `last`, or a unique title prefix")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if *thread == "" && len(rest) == 1 {
		*thread = rest[0]
		rest = nil
	}
	if len(rest) != 0 {
		return usagef("ui open takes only --thread (got %v)", rest)
	}
	if *thread == "" {
		return usagef("ui open needs --thread <id|#N|last|title-prefix>")
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		row, err := resolveThreadSelector(ctx, client, *thread)
		if err != nil {
			return err
		}
		// The production open path, driven from outside the browser: this
		// is the channel an OS-notification click rides, so the SPA runs
		// its own openThreadInPane rather than anything harness-shaped.
		if err := openThreadOnPage(ctx, e, client, row.ID); err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{"threadId": row.ID, "opened": true})
		}
		e.printf("opened %s (%s)\n", row.ID, truncate(row.Title, 60))
		return nil
	})
}

// queryUI is the one call every ui verb makes: a HarnessUIQuery carrying a
// spec object the BRIDGE defines. The CLI builds the JSON rather than a
// typed struct so a bridge that grows a field needs no release here.
func (e *env) queryUI(ctx context.Context, client *harnessclient.Client, spec map[string]any) (json.RawMessage, error) {
	spec["v"] = 1
	raw, err := client.Call(ctx, "HarnessUIQuery", spec)
	if err != nil {
		return nil, uiQueryError(err)
	}
	return raw, nil
}

// uiQueryError rewrites the backend's timeout into the sentence that names
// the fix. "No frontend attached" is almost always "you booted headless",
// and the answer is a window or a browser on the instance URL.
func uiQueryError(err error) error {
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "no frontend attached") {
		return err
	}
	return fmt.Errorf("%w\n"+
		"  the bridge answers only while a page is open on this instance:\n"+
		"    make harness-window   (opens the real webview on this worktree's harness)\n"+
		"    ao-harness open       (prints the URL to open in a browser)", err)
}

func uiSnapshot(e *env, args []string) error {
	flags := e.newFlagSet("ui snapshot")
	pane := flags.String("pane", "", "print only this pane id")
	settledMs := flags.Int("settled-ms", 0, "how long the DOM must be quiet to report settled (bridge default: 300)")
	textHead := flags.Int("text-head", 0, "characters of each row's text to include (bridge default)")
	// No backquotes in this string: flag.PrintDefaults reads the first
	// backquoted word as the value PLACEHOLDER, so "`ui diff`" turned the
	// flag's own help into "-save ui diff" — a name for the argument
	// rather than a reference to another command.
	save := flags.Bool("save", true, "store this snapshot as the baseline that 'ui diff' compares against")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("ui snapshot takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, t target, _ harnessclient.Bootstrap) error {
		view, raw, err := e.takeViewport(ctx, client, *settledMs, *textHead)
		if err != nil {
			return err
		}
		if *save {
			if err := writeUISnapshot(t, view); err != nil {
				return err
			}
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		e.renderViewport(view, *pane)
		return nil
	})
}

func (e *env) takeViewport(ctx context.Context, client *harnessclient.Client, settledMs, textHead int) (uiViewport, json.RawMessage, error) {
	spec := map[string]any{"kind": "viewport"}
	if settledMs > 0 {
		spec["settledMs"] = settledMs
	}
	if textHead > 0 {
		spec["textHead"] = textHead
	}
	raw, err := e.queryUI(ctx, client, spec)
	if err != nil {
		return uiViewport{}, nil, err
	}
	view, err := decodeViewport(raw)
	if err != nil {
		return uiViewport{}, nil, err
	}
	return view, raw, nil
}

func (e *env) renderViewport(view uiViewport, only string) {
	e.printf("thread %s  settled=%t (quiet %.0fms)  dom=%d  panes=%d  overlays=%d\n",
		orDash(view.ActiveThreadID), view.Settled, view.SinceMutationMs,
		view.DomNodes, len(view.Panes), len(view.Overlays))
	for _, overlay := range view.Overlays {
		e.printf("  overlay %s %q\n", overlay.Kind, overlay.Name)
	}
	for _, pane := range view.Panes {
		if only != "" && pane.PaneID != only {
			continue
		}
		focus := ""
		if pane.Focused {
			focus = " focused"
		}
		e.printf("\npane %s (%s%s) thread=%s mounted=%d\n",
			pane.PaneID, pane.PaneKind, focus, orDash(pane.ThreadID), pane.MountedRows)
		if pane.Scroll != nil {
			e.printf("  scroll top=%.0f height=%.0f client=%.0f fromBottom=%.0f atBottom=%t\n",
				pane.Scroll.Top, pane.Scroll.Height, pane.Scroll.Client,
				pane.Scroll.DistanceFromBottom, pane.Scroll.AtBottom)
		}
		if len(pane.Rows) == 0 {
			continue
		}
		rows := make([][]string, 0, len(pane.Rows))
		for _, row := range pane.Rows {
			view := "-"
			if row.InViewport {
				view = "vis"
			}
			rows = append(rows, []string{
				fmt.Sprint(row.RowIndex), truncate(row.ItemID, 22), row.Kind, row.Role,
				rowStateLabel(row), view,
				fmt.Sprintf("%.0fx%.0f@%.0f", row.Rect.W, row.Rect.H, row.Rect.Y),
				truncate(row.TextHead, 48),
			})
		}
		_ = e.table([]string{"  #", "ITEM", "KIND", "ROLE", "STATE", "VIEW", "RECT", "TEXT"}, rows)
	}
}

func uiQuery(e *env, args []string) error {
	flags := e.newFlagSet("ui query <selector>")
	textCap := flags.Int("text-cap", 0, "characters of textContent to include (bridge default)")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("ui query needs exactly one CSS selector (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		spec := map[string]any{"kind": "element", "selector": rest[0]}
		if *textCap > 0 {
			spec["textCap"] = *textCap
		}
		raw, err := e.queryUI(ctx, client, spec)
		if err != nil {
			return err
		}
		if e.jsonOutput() {
			return e.writeRawJSON(raw)
		}
		var result struct {
			Selector string `json:"selector"`
			Count    int    `json:"count"`
			First    *struct {
				Tag       string `json:"tag"`
				Rect      uiRect `json:"rect"`
				Visible   bool   `json:"visible"`
				Clipped   bool   `json:"clipped"`
				Text      string `json:"text"`
				Role      string `json:"role"`
				AriaLabel string `json:"ariaLabel"`
				TestID    string `json:"testId"`
			} `json:"first"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode element result: %w", err)
		}
		e.printf("%s: %d match(es)\n", result.Selector, result.Count)
		if result.First == nil {
			return nil
		}
		first := result.First
		e.printf("  <%s> %.0fx%.0f at (%.0f, %.0f) visible=%t clipped=%t\n",
			first.Tag, first.Rect.W, first.Rect.H, first.Rect.X, first.Rect.Y,
			first.Visible, first.Clipped)
		if first.Role != "" || first.AriaLabel != "" || first.TestID != "" {
			e.printf("  role=%s aria=%q testid=%s\n", orDash(first.Role), first.AriaLabel, orDash(first.TestID))
		}
		if first.Text != "" {
			e.printf("  text: %s\n", truncate(first.Text, 200))
		}
		return nil
	})
}

func uiState(e *env, args []string) error {
	flags := e.newFlagSet("ui state <name> [json args...]")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usagef("ui state needs a global name (e.g. __stickState, uiTrace.recent)")
	}
	name := rest[0]
	callArgs := make([]any, 0, len(rest)-1)
	for i, arg := range rest[1:] {
		if !json.Valid([]byte(arg)) {
			return usagef("argument %d (%q) is not a JSON value; numbers are bare, strings need quotes", i+1, arg)
		}
		callArgs = append(callArgs, json.RawMessage(arg))
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		spec := map[string]any{"kind": "globals", "name": name}
		if len(callArgs) > 0 {
			spec["args"] = callArgs
		}
		raw, err := e.queryUI(ctx, client, spec)
		if err != nil {
			return err
		}
		return e.writeRawJSON(raw)
	})
}

func uiDiffCommand(e *env, args []string) error {
	flags := e.newFlagSet("ui diff")
	threshold := flags.Float64("threshold", uiGeometryThresholdPx, "report a geometry delta at or above this many pixels")
	settledMs := flags.Int("settled-ms", 0, "how long the DOM must be quiet to report settled")
	save := flags.Bool("save", true, "store the new snapshot as the next diff's baseline")
	rest, err := e.parse(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("ui diff takes no positional arguments (got %v)", rest)
	}
	ctx := context.Background()
	return e.withClient(ctx, func(client *harnessclient.Client, t target, _ harnessclient.Bootstrap) error {
		previous, err := readUISnapshot(t)
		if err != nil {
			return err
		}
		view, _, err := e.takeViewport(ctx, client, *settledMs, 0)
		if err != nil {
			return err
		}
		current := uiSnapshotFile{
			TakenAt:  time.Now().Format(time.RFC3339),
			Instance: t.ID,
			Viewport: view,
		}
		if *save {
			if err := writeUISnapshot(t, view); err != nil {
				return err
			}
		}
		diff := diffViewports(previous.Viewport, current.Viewport, *threshold)
		if e.jsonOutput() {
			return e.writeJSON(map[string]any{
				"before": previous.TakenAt,
				"after":  current.TakenAt,
				"diff":   diff,
			})
		}
		e.printf("%s", renderUIDiff(diff, previous, current))
		return nil
	})
}

func uiSnapshotPath(t target) string {
	return filepath.Join(t.DataDir, uiSnapshotDirName, uiSnapshotFileName)
}

func writeUISnapshot(t target, view uiViewport) error {
	path := uiSnapshotPath(t)
	if err := atomicfile.WriteJSON(path, uiSnapshotFile{
		TakenAt:  time.Now().Format(time.RFC3339),
		Instance: t.ID,
		Viewport: view,
	}); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readUISnapshot loads the baseline `ui diff` compares against. The
// viewport goes through decodeViewport's version check exactly as a live
// reply does: a file written by an older bridge is the same "field names
// moved, everything decoded to zero" trap, and it is easier to hit —
// nothing rewrites the file when the app is upgraded, so a stale baseline
// outlives the shape it was taken from.
func readUISnapshot(t target) (uiSnapshotFile, error) {
	path := uiSnapshotPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return uiSnapshotFile{}, fmt.Errorf(
				"no previous snapshot for instance %s; run `ao-harness ui snapshot` first (it writes %s)", t.ID, path)
		}
		return uiSnapshotFile{}, err
	}
	var out struct {
		TakenAt  string          `json:"takenAt"`
		Instance string          `json:"instance"`
		Viewport json.RawMessage `json:"viewport"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return uiSnapshotFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	view, err := decodeViewport(out.Viewport)
	if err != nil {
		return uiSnapshotFile{}, fmt.Errorf("read %s: %w (delete it and take a fresh snapshot)", path, err)
	}
	return uiSnapshotFile{TakenAt: out.TakenAt, Instance: out.Instance, Viewport: view}, nil
}
