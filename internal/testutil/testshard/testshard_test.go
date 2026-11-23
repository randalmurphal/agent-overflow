package testshard

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/testshard/shardspec"
)

// childEnv marks a copy of this test binary that TestSelectRunsEveryTestOfTheBinaryInOneShard
// started, so its own run of that test does not start more.
const childEnv = "AO_TESTSHARD_CHILD"

func TestMain(m *testing.M) {
	Select(m)
	os.Exit(m.Run())
}

func TestParse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		spec         string
		shard, count int
	}{
		{"0/1", 0, 1},
		{"0/4", 0, 4},
		{"3/4", 3, 4},
		{"23/24", 23, 24},
	} {
		shard, count, err := shardspec.Parse(tc.spec)
		if err != nil || shard != tc.shard || count != tc.count {
			t.Errorf("shardspec.Parse(%q) = %d, %d, %v; want %d, %d", tc.spec, shard, count, err, tc.shard, tc.count)
		}
	}
	for _, spec := range []string{"", "1", "/", "0/", "/4", "4/4", "5/4", "0/0", "-1/4", "+1/4", "1/+4", " 1/4", "1/4 ", "1/4/8", "a/4", "1/b", "0x1/4"} {
		if _, _, err := shardspec.Parse(spec); err == nil {
			t.Errorf("shardspec.Parse(%q) succeeded; want an error", spec)
		} else if !strings.Contains(err.Error(), shardspec.Env) {
			t.Errorf("shardspec.Parse(%q) error %q does not name %s", spec, err, shardspec.Env)
		}
	}
}

// TestEveryNameRunsInExactlyOneShard checks the -run expressions Select
// installs: for each shard count, every name is matched by exactly one
// shard's expression, and adding names moves none of the earlier ones.
func TestEveryNameRunsInExactlyOneShard(t *testing.T) {
	t.Parallel()
	names := generatedNames(0, 400)
	added := append(append([]string(nil), names...), generatedNames(400, 100)...)
	for _, count := range []int{1, 2, 3, 4, 7, 24, 64} {
		before := shardsMatching(t, names, count)
		after := shardsMatching(t, added, count)
		for _, name := range names {
			if before[name] != after[name] {
				t.Errorf("K=%d: adding tests moved %s from shard %d to %d", count, name, before[name], after[name])
			}
		}
	}
}

// shardsMatching compiles every shard's expression over names and returns
// the one shard each name matches, failing for a name matched by none or
// by more than one.
func shardsMatching(t *testing.T, names []string, count int) map[string]int {
	t.Helper()
	patterns := make([]*regexp.Regexp, count)
	for shard := range count {
		patterns[shard] = regexp.MustCompile(pattern(names, shard, count))
	}
	got := make(map[string]int, len(names))
	for _, name := range names {
		var matched []int
		for shard, pattern := range patterns {
			if pattern.MatchString(name) {
				matched = append(matched, shard)
			}
			// A name that only shares a prefix or a suffix with a
			// selected one is not selected.
			if pattern.MatchString(name+"X") || pattern.MatchString("X"+name) {
				t.Fatalf("K=%d: shard %d's expression matches a longer name than %s", count, shard, name)
			}
		}
		if len(matched) != 1 {
			t.Fatalf("K=%d: %s matches shards %v; want exactly one", count, name, matched)
		}
		got[name] = matched[0]
	}
	return got
}

func generatedNames(from, n int) []string {
	names := make([]string, 0, n)
	for i := from; i < from+n; i++ {
		// Names that prefix one another, as TestA and TestAB do.
		names = append(names, fmt.Sprintf("TestGenerated%d", i))
	}
	return names
}

// TestSelectRunsEveryTestOfTheBinaryInOneShard runs this binary once per
// shard for several shard counts and checks that every test, example and
// fuzz target it lists ran in exactly one of them. A test added to this
// file is in the list.
func TestSelectRunsEveryTestOfTheBinaryInOneShard(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		return
	}
	t.Parallel()
	listing, err := runChild(t, nil, "-test.list=.")
	if err != nil {
		t.Fatalf("list the tests: %v\n%s", err, listing)
	}
	var all []string
	for line := range strings.Lines(string(listing)) {
		if name := strings.TrimSpace(line); name != "" && !strings.HasPrefix(name, "ok ") {
			all = append(all, name)
		}
	}
	for _, want := range []string{"TestParse", "TestARunNarrowedByRunIsWhole", "Example_pattern", "FuzzShardOf"} {
		if !slices.Contains(all, want) {
			t.Fatalf("the listing %q lacks %s", all, want)
		}
	}
	for _, count := range []int{1, 2, 3, 5} {
		ran := map[string]int{}
		for shard := range count {
			out, err := runChild(t, []string{fmt.Sprintf("%s=%d/%d", shardspec.Env, shard, count)}, "-test.v=true")
			if err != nil {
				t.Fatalf("K=%d shard %d: %v\n%s", count, shard, err, out)
			}
			scanner := bufio.NewScanner(bytes.NewReader(out))
			for scanner.Scan() {
				name, ok := strings.CutPrefix(scanner.Text(), "=== RUN   ")
				if ok && !strings.Contains(name, "/") {
					ran[name]++
				}
			}
		}
		for _, name := range all {
			if ran[name] != 1 {
				t.Errorf("K=%d: %s ran in %d shards; want 1", count, name, ran[name])
			}
		}
		if len(ran) != len(all) {
			t.Errorf("K=%d: ran %v; the binary lists %v", count, ran, all)
		}
	}
}

func TestSelectRefusesAMalformedShard(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		return
	}
	t.Parallel()
	for _, tc := range []struct {
		env  string
		args []string
		want string
	}{
		{env: "2/2", want: shardspec.Env + `="2/2"`},
		{env: "", want: shardspec.Env + `=""`},
		{env: "x", args: []string{"-test.run=TestParse"}, want: shardspec.Env + `="x"`},
	} {
		out, err := runChild(t, []string{shardspec.Env + "=" + tc.env}, tc.args...)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("%s=%q %v: err %v; want exit status 2\n%s", shardspec.Env, tc.env, tc.args, err, out)
			continue
		}
		if !strings.Contains(string(out), tc.want) || strings.Contains(string(out), "=== RUN") {
			t.Errorf("%s=%q %v: output %q; want %q before any test runs", shardspec.Env, tc.env, tc.args, out, tc.want)
		}
	}
}

// TestARunNarrowedByRunIsWhole runs a test, as a helper process started
// from a test's binary would, with -run and the shard variable it
// inherited, and checks that every test -run names ran, whatever its
// shard.
func TestARunNarrowedByRunIsWhole(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		return
	}
	t.Parallel()
	for shard := range 3 {
		out, err := runChild(t, []string{fmt.Sprintf("%s=%d/3", shardspec.Env, shard)}, "-test.v=true", "-test.run=^(TestParse|TestEveryNameRunsInExactlyOneShard)$")
		if err != nil {
			t.Fatalf("shard %d: %v\n%s", shard, err, out)
		}
		for _, name := range []string{"TestParse", "TestEveryNameRunsInExactlyOneShard"} {
			if !strings.Contains(string(out), "=== RUN   "+name+"\n") {
				t.Errorf("shard %d with -run: %s did not run\n%s", shard, name, out)
			}
		}
	}
}

func Example_pattern() {
	fmt.Println(pattern([]string{"TestA"}, 0, 1))
	// Output: ^(?:TestA)$
}

func FuzzShardOf(f *testing.F) {
	f.Add("TestA", 3)
	f.Fuzz(func(t *testing.T, name string, count int) {
		if count < 1 {
			return
		}
		if shard := of(name, count); shard < 0 || shard >= count {
			t.Fatalf("of(%q, %d) = %d", name, count, shard)
		}
	})
}

func runChild(t *testing.T, env []string, args ...string) ([]byte, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], args...)
	command.Env = append(append(withoutShard(os.Environ()), childEnv+"=1"), env...)
	return command.CombinedOutput()
}

func withoutShard(environ []string) []string {
	var kept []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, shardspec.Env+"=") {
			kept = append(kept, kv)
		}
	}
	return kept
}
