//go:build linux || darwin

package browser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// opsView is a nativeView whose answers a test sets: evaluate answers
// every body with result, and the rest answer as their fields say. steps
// records the history steps taken.
type opsView struct {
	mu      sync.Mutex
	result  string
	evalErr error
	can     bool
	canErr  error
	loading bool
	loadErr error
	steps   []nativeHistoryStep
}

func (v *opsView) evaluate(ctx context.Context, _, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.result, v.evalErr
}

func (v *opsView) canGo(bool) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.can, v.canErr
}

func (v *opsView) isLoading() (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.loading, v.loadErr
}

func (v *opsView) history(step nativeHistoryStep) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.steps = append(v.steps, step)
	return nil
}

func newOps(t *testing.T, view *opsView) *nativePageOps {
	t.Helper()
	return &nativePageOps{view: view, life: t.Context()}
}

// A view that cannot answer for its history or loading state fails the
// operation that asked, rather than reading as no history or as loaded.
func TestNativePageOpsReportViewFailures(t *testing.T) {
	gone := errors.New("the view is gone")
	t.Run("history state", func(t *testing.T) {
		ops := newOps(t, &opsView{canErr: gone})
		if _, _, err := ops.HistoryState(t.Context()); !errors.Is(err, gone) {
			t.Fatalf("HistoryState = %v", err)
		}
	})
	t.Run("history step", func(t *testing.T) {
		view := &opsView{canErr: gone}
		if err := newOps(t, view).History(t.Context(), "back"); !errors.Is(err, gone) {
			t.Fatalf("History(back) = %v", err)
		}
		if len(view.steps) != 0 {
			t.Fatalf("a step was taken without a readable history: %v", view.steps)
		}
	})
	t.Run("load wait", func(t *testing.T) {
		ops := newOps(t, &opsView{loadErr: gone})
		if err := ops.awaitLoad(t.Context()); !errors.Is(err, gone) {
			t.Fatalf("awaitLoad = %v", err)
		}
	})
	t.Run("page status", func(t *testing.T) {
		ops := newOps(t, &opsView{result: `{"URL":"https://example.test/","Ready":"complete"}`, loadErr: gone})
		if _, err := ops.PageStatus(t.Context()); !errors.Is(err, gone) {
			t.Fatalf("PageStatus = %v", err)
		}
	})
	t.Run("selection", func(t *testing.T) {
		ops := newOps(t, &opsView{evalErr: gone})
		if _, err := ops.SelectionText(t.Context()); !errors.Is(err, gone) {
			t.Fatalf("SelectionText = %v", err)
		}
	})
}

func TestNativePageOpsHistory(t *testing.T) {
	view := &opsView{}
	ops := newOps(t, view)
	if err := ops.History(t.Context(), "back"); err == nil || !strings.Contains(err.Error(), "no previous history entry") {
		t.Fatalf("back with no history = %v", err)
	}
	if err := ops.History(t.Context(), "forward"); err == nil || !strings.Contains(err.Error(), "no forward history entry") {
		t.Fatalf("forward with no history = %v", err)
	}
	view.can = true
	for _, action := range []string{"back", "forward", "reload", "stop"} {
		if err := ops.History(t.Context(), action); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	want := []nativeHistoryStep{nativeHistoryBack, nativeHistoryForward, nativeHistoryReload, nativeHistoryStop}
	if len(view.steps) != len(want) {
		t.Fatalf("steps = %v, want %v", view.steps, want)
	}
	for i := range want {
		if view.steps[i] != want[i] {
			t.Fatalf("steps = %v, want %v", view.steps, want)
		}
	}
	if err := ops.History(t.Context(), "sideways"); err == nil {
		t.Fatal("an unknown history action was accepted")
	}
}

// An operation ends with the page's lifetime as well as its caller's.
func TestNativePageOpsEndWithThePage(t *testing.T) {
	life, closePage := context.WithCancel(t.Context())
	ops := &nativePageOps{view: &opsView{loading: true}, life: life}
	closePage()
	if err := ops.awaitLoad(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a load wait on a closed page = %v", err)
	}
	if _, err := ops.evalBody(t.Context(), "return 1;"); !errors.Is(err, context.Canceled) {
		t.Fatalf("an evaluation on a closed page = %v", err)
	}
}

func TestNativePageOpsDecodeResults(t *testing.T) {
	view := &opsView{result: "null"}
	ops := newOps(t, view)
	if raw, err := ops.evalBody(t.Context(), "return null;"); err != nil || raw != nil {
		t.Fatalf("a null result = %q, %v", raw, err)
	}
	view.result = `"picked text"`
	if selected, err := ops.SelectionText(t.Context()); err != nil || selected != "picked text" {
		t.Fatalf("SelectionText = %q, %v", selected, err)
	}
	view.result = `{"contentType":" text/plain ","base64":"aGVsbG8="}`
	fetch, err := ops.AssetFetcher(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := fetch("https://example.test/a.txt")
	if err != nil || asset.ContentType != "text/plain" {
		t.Fatalf("asset = %+v, %v", asset, err)
	}
	var out strings.Builder
	if _, err := asset.Copy(&out, 4, 100); err == nil {
		t.Fatal("an asset past the per-file limit was copied")
	}
	if n, err := asset.Copy(&out, 5, 5); err != nil || n != 5 || out.String() != "hello" {
		t.Fatalf("copy = %d, %q, %v", n, out.String(), err)
	}
}
