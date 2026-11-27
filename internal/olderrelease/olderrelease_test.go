package olderrelease

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/transport"
)

// The frozen file is the tag's method table, name for name, whenever the
// checkout holds the tag; it parses and names a tag either way.
func TestTheFrozenReleaseIsItsTagsMethodTable(t *testing.T) {
	frozen, err := Parse(methodsFile)
	if err != nil {
		t.Fatalf("methods.txt: %v", err)
	}
	if Format(frozen) != methodsFile {
		t.Errorf("methods.txt is not as `make older-release TAG=%s` writes it", frozen.Tag)
	}
	dir := reporoot.Dir(t)
	if err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", frozen.Tag+"^{commit}").Run(); err != nil {
		t.Skipf("the checkout does not hold the tag %s (%v), so methods.txt is not compared with it", frozen.Tag, err)
	}
	src, err := exec.Command("git", "-C", dir, "show", frozen.Tag+":internal/transport/methods_gen.go").Output()
	if err != nil {
		t.Fatalf("read the method table of %s: %v", frozen.Tag, err)
	}
	names, err := TableNames(src)
	if err != nil {
		t.Fatalf("%s: %v", frozen.Tag, err)
	}
	if !slices.Equal(names, frozen.Methods) {
		t.Errorf("methods.txt names %d methods, the table of %s %d; write it again with `make older-release TAG=%s`:\n%s",
			len(frozen.Methods), frozen.Tag, len(names), frozen.Tag, difference(frozen.Methods, names))
	}
}

// TableNames reads this build's table as the transport has it.
func TestTableNamesReadsTheGeneratedTable(t *testing.T) {
	src, err := os.ReadFile(reporoot.Path(t, "internal/transport/methods_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := TableNames(src)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, m := range transport.GeneratedMethods {
		want = append(want, m.Name)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("TableNames read %d methods, the transport binds %d:\n%s", len(got), len(want), difference(got, want))
	}
}

// A methods file names its tag and each method once, in order.
func TestParseRefusesAMalformedFile(t *testing.T) {
	for _, text := range []string{
		"A\nB\n",
		"# tag v1\n",
		"# tag v1\nB\nA\n",
		"# tag v1\nA\nA\n",
		"# tag v1\nnot a name\n",
	} {
		if _, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) took it", text)
		}
	}
	r, err := Parse("# a comment\n# tag v1\nA\nB\n")
	if err != nil || r.Tag != "v1" || !slices.Equal(r.Methods, []string{"A", "B"}) || !r.Has("B") || r.Has("C") {
		t.Errorf("Parse answered %+v, %v", r, err)
	}
}

// difference names what each of two sorted lists holds that the other
// does not.
func difference(have, want []string) string {
	var b strings.Builder
	for _, n := range have {
		if _, ok := slices.BinarySearch(want, n); !ok {
			b.WriteString("  only in the first: " + n + "\n")
		}
	}
	for _, n := range want {
		if _, ok := slices.BinarySearch(have, n); !ok {
			b.WriteString("  only in the second: " + n + "\n")
		}
	}
	return b.String()
}
