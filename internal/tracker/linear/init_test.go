package linear

import (
	"strconv"
	"strings"
	"testing"
)

// A family document is made on its first use, once: later uses return
// it with no allocation, and the single-issue read's history page is
// the constant written into its document.
func TestFamilyDocumentMadeOnce(t *testing.T) {
	first := renumberFamily.op(3)
	if again := renumberFamily.op(3); again.text != first.text || len(first.results) != 3 {
		t.Fatalf("op(3) %q then %q", first.results, again.results)
	}
	if !raceOn {
		if n := testing.AllocsPerRun(100, func() { _ = renumberFamily.op(3) }); n != 0 {
			t.Errorf("a made document costs %v allocations a use", n)
		}
	}
	if want := "history(first: " + strconv.Itoa(detailHistoryPage) + ")"; !strings.Contains(opIssue.text, want) {
		t.Errorf("the Issue document does not hold %s", want)
	}
}

// BenchmarkFamilyOp is the cost of a family document once made, against
// the slice index it replaced.
func BenchmarkFamilyOp(b *testing.B) {
	b.Run("once", func(b *testing.B) {
		_ = renumberFamily.op(maxBatch)
		for b.Loop() {
			_ = renumberFamily.op(maxBatch)
		}
	})
	b.Run("slice index", func(b *testing.B) {
		docs := make([]operation, maxBatch)
		docs[maxBatch-1] = renumberFamily.op(maxBatch)
		op := func(n int) operation {
			if n < 1 || n > len(docs) {
				panic("outside")
			}
			return docs[n-1]
		}
		for b.Loop() {
			_ = op(maxBatch)
		}
	})
}
