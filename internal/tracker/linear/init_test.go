package linear

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The package allocates nothing at init: every app process links it and
// most never reach Linear. Its init does run code, which the trace shows
// as a line: each operation variable is the value its constructor
// returns, registered in a fixed array for the schema test, and a family
// document is made on its first use (family.op). The probe in testdata
// links the package and nothing else of this module, so its init trace is
// the package's own, free of the test variables a test binary adds.
func TestInitCost(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "initcost")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", bin, "./testdata/initcost")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build the probe: %v\n%s", err, out)
	}
	run := exec.CommandContext(t.Context(), bin)
	run.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("run the probe: %v\n%s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "init agent-overflow/internal/tracker/linear ") && !strings.HasSuffix(line, " 0 bytes, 0 allocs") {
			t.Errorf("the package allocates at init: %s", line)
		}
	}
}

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
