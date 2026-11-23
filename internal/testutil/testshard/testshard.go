// Package testshard splits one test package's run across processes.
//
// A package opts in by calling Select first in its TestMain. `make go-test`
// (cmd/ao-gotest) then runs it as one `go test` per core with
// shardspec.Env set to "i/K" in each, and every top-level test runs in
// exactly one of them, the one its name hashes to. Without the variable the
// package runs whole, as any other. Select is all this package exports, so
// a package that imports it calls it.
//
// The reason is modernc.org/sqlite's C allocator: every malloc and free in
// the process takes one lock, and opening a connection, parsing its schema
// and compiling statements are malloc-heavy, so the parallel tests of one
// process queue on that lock and a package whose tests open many stores
// stops scaling past a few cores. Separate processes do not share it.
//
// A package that no longer needs it drops the Select call; nothing else
// names it.
package testshard

import (
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/testshard/shardspec"
)

// Select restricts m's run to the tests of the shard shardspec.Env names,
// and does nothing when it is unset. It fails the process, before any test
// runs, when the value is malformed, when -run is also given (cmd/ao-gotest
// never splits a run that -run narrows), or when this Go version's
// testing.M no longer has the lists Select reads.
func Select(m *testing.M) {
	spec, ok := os.LookupEnv(shardspec.Env)
	if !ok {
		return
	}
	if err := selectShard(m, spec); err != nil {
		fmt.Fprintf(os.Stderr, "testshard: %v\n", err)
		os.Exit(2)
	}
}

func selectShard(m *testing.M, spec string) error {
	shard, count, err := shardspec.Parse(spec)
	if err != nil {
		return err
	}
	names, err := testNames(m)
	if err != nil {
		return err
	}
	if !flag.Parsed() {
		flag.Parse()
	}
	run := flag.Lookup("test.run")
	if run == nil {
		return fmt.Errorf("no -test.run flag to restrict (Go %s)", runtime.Version())
	}
	if given := run.Value.String(); given != "" {
		return fmt.Errorf("%s=%s with -run %q: a shard runs its share of every test, so narrow a run with -run alone", shardspec.Env, spec, given)
	}
	return run.Value.Set(pattern(names, shard, count))
}

// of is the shard of count that the top-level test named name runs in. It
// depends on the name alone, so adding or removing a test moves no other.
func of(name string, count int) int {
	h := fnv.New64a()
	h.Write([]byte(name))
	return int(h.Sum64() % uint64(count))
}

// pattern is the -run expression that selects, among names, the tests of
// shard i of count. It matches only whole top-level names, so a selected
// test runs all of its subtests. A shard with no tests gets an expression
// no test name matches.
func pattern(names []string, shard, count int) string {
	var mine []string
	for _, name := range names {
		if of(name, count) == shard {
			mine = append(mine, regexp.QuoteMeta(name))
		}
	}
	return "^(?:" + strings.Join(mine, "|") + ")$"
}

// testNames lists what -run selects among: the tests, examples and fuzz
// targets of m. testing.M does not export them; reading them is what lets
// a shard be chosen by name without the package listing its tests.
func testNames(m *testing.M) ([]string, error) {
	v := reflect.ValueOf(m).Elem()
	var names []string
	for _, field := range []string{"tests", "examples", "fuzzTargets"} {
		list := v.FieldByName(field)
		if list.Kind() != reflect.Slice {
			return nil, fmt.Errorf("testing.M has no %s list in Go %s; update testshard", field, runtime.Version())
		}
		for i := range list.Len() {
			name := list.Index(i).FieldByName("Name")
			if name.Kind() != reflect.String {
				return nil, fmt.Errorf("testing.M's %s have no Name in Go %s; update testshard", field, runtime.Version())
			}
			names = append(names, name.String())
		}
	}
	return names, nil
}
