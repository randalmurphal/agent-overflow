package adf

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentUseLeavesInputs proves the converter is safe for
// concurrent use and reads its inputs only: goroutines converting and
// editing the same golden documents at once get the results one
// goroutine gets alone, and every original document is unchanged after.
// Run with -race, a shared write fails it.
func TestConcurrentUseLeavesInputs(t *testing.T) {
	type editCase struct {
		orig *Node
		md   string
	}
	var edits []editCase
	var mds []string
	paths, err := filepath.Glob(filepath.Join(goldenDir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".md")
		md := readGolden(t, name, ".md")
		switch {
		case strings.HasPrefix(name, "edit-"):
			edits = append(edits, editCase{parseGoldenJSON(t, readGolden(t, name, ".orig.json")), md})
		case strings.HasPrefix(name, "rt-"), strings.HasPrefix(name, "write-"):
			mds = append(mds, md)
		}
	}
	if len(edits) == 0 || len(mds) == 0 {
		t.Fatal("no golden inputs")
	}
	before := make([]string, len(edits))
	for i, c := range edits {
		before[i] = mustJSON(c.orig)
	}
	run := func() []string {
		ctx := context.Background()
		var out []string
		for _, c := range edits {
			doc, ws, err := Edit(ctx, c.orig, c.md, newFakeResolver())
			out = append(out, mustJSON(doc)+warnText(ws)+errText(err), ToMarkdown(c.orig))
		}
		for _, md := range mds {
			doc, ws, err := FromMarkdown(ctx, md, newFakeResolver())
			out = append(out, mustJSON(doc)+warnText(ws)+errText(err), ToMarkdown(doc))
		}
		return out
	}
	want := run()
	const workers = 8
	got := make([][]string, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[w] = run()
		}()
	}
	wg.Wait()
	for w, g := range got {
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("worker %d, result %d differs from the sequential run", w, i)
			}
		}
	}
	for i, c := range edits {
		if mustJSON(c.orig) != before[i] {
			t.Errorf("Edit changed original %d", i)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
