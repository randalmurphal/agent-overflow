package trackerapp

import (
	"go/parser"
	gotoken "go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// initCost is what a package's init may do. With runs false it runs no
// code, so the trace has no line for it; with runs true it may run code,
// within bytes and allocs.
type initCost struct {
	runs          bool
	bytes, allocs int
}

// Every app process links these packages and most have no tracker
// connection, so none of them does work at start: tables and expressions
// are built on first use.
var initCosts = map[string]initCost{
	// ErrInvalidInput is trackerapi's error, a variable of another
	// package, copied when the package initializes so errors.Is matches
	// either name.
	"agent-overflow/internal/tracker": {runs: true},
	// Its one piece of work, registering a goldmark node kind, appends to
	// goldmark's table within the room that table has (measured: 0
	// bytes). A goldmark release whose table grows at this registration
	// fails the test; raise bytes to that growth then, and nothing else.
	"agent-overflow/internal/tracker/adf": {runs: true},
	// Its expressions are matchers (shapes.go) and its tables arrays or
	// switches.
	"agent-overflow/internal/tracker/jira": {},
	// An issue key's shape is jira.IsIssueKey.
	"agent-overflow/internal/tracker/jiraprovider": {},
	// Each operation variable is the value its constructor returns,
	// registered in a fixed array for the schema test, and a family
	// document is made on its first use (family.op).
	"agent-overflow/internal/tracker/linear": {runs: true},
	// The estimate scales are static values.
	"agent-overflow/internal/tracker/linearprovider": {},
	"agent-overflow/internal/tracker/tempo":          {},
	"agent-overflow/internal/tracker/trackerapi":     {},
	"agent-overflow/internal/trackerapp":             {runs: true},
}

var initTrace = regexp.MustCompile(`^init (\S+) @.*, (\d+) bytes, (\d+) allocs$`)

// Each package in initCosts keeps to its cost at init. The probe in
// testdata imports every one of them and nothing else of this module
// beyond what they import, so one build gives each package's own trace
// line, free of the test variables a test binary adds.
func TestInitCost(t *testing.T) {
	t.Parallel()
	const probe = "testdata/initcost"
	f, err := parser.ParseFile(gotoken.NewFileSet(), filepath.Join(probe, "main.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	imported := map[string]bool{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		imported[path] = true
	}
	for pkg := range initCosts {
		if !imported[pkg] {
			t.Errorf("the probe does not import %s, so its init goes unchecked", pkg)
		}
	}

	bin := filepath.Join(t.TempDir(), "initcost")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", bin, "./"+probe)
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
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "init" {
			continue
		}
		limit, ok := initCosts[fields[1]]
		if !ok {
			continue
		}
		if !limit.runs {
			t.Errorf("%s runs code at init: %s", fields[1], line)
			continue
		}
		m := initTrace.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("an init trace line of another shape: %s", line)
		}
		bytes, _ := strconv.Atoi(m[2])
		allocs, _ := strconv.Atoi(m[3])
		if bytes > limit.bytes || allocs > limit.allocs {
			t.Errorf("%s allocates %d bytes in %d allocations at init, over %d and %d: %s",
				fields[1], bytes, allocs, limit.bytes, limit.allocs, line)
		}
	}
}

// isLinearURLKey matches exactly what the expression it replaced does.
func TestLinearURLKeyShape(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	keys := []string{"", "a", "0", "-", "-a", "a-", "a--b", "acme", "Acme", "a b", "a_b", "a.b", "a\n", "\na", "é", "a\xff",
		strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 65), strings.Repeat("-", 64), "a" + strings.Repeat("-", 63)}
	for b := range 256 {
		keys = append(keys, string([]byte{byte(b)}), string([]byte{'a', byte(b)}), string([]byte{byte(b), 'a'}))
	}
	for _, k := range keys {
		if got, want := isLinearURLKey(k), shape.MatchString(k); got != want {
			t.Errorf("isLinearURLKey(%q) = %v, the expression says %v", k, got, want)
		}
	}
}
