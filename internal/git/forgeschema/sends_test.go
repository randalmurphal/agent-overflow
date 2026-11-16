package forgeschema

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Sends follows a send's arguments to package constants however the
// document text opens, and refuses an argument it cannot follow.
func TestSends(t *testing.T) {
	t.Parallel()
	trace := func(src string, tables map[string]any) ([]string, error) {
		dir := t.TempDir()
		const head = "package a\n\nvar c interface{ GraphQL(ctx, op, query, vars, out any) }\n\n" +
			"const githubA = `# a comment first\\nquery A { viewer { login } }`\n\n" +
			"const githubB = `fragment F on User { login }\\nquery B { viewer { ...F } }`\n\n"
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(head+src+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sends, _, err := Sends(dir, tables)
		var out []string
		for _, s := range sends {
			out = append(out, s.Operation+" "+s.Name+" "+s.Forge)
		}
		return out, err
	}
	got, err := trace(`
func pick(b bool) {
	op, doc := "A", githubA
	if b {
		op, doc = "B", githubB
	}
	send(op, doc)
}

func send(op, doc string) { c.GraphQL(nil, op, doc, nil, nil) }

func each() {
	step := func(op, doc string) { c.GraphQL(nil, op, doc, nil, nil) }
	step("A", githubA)
	for _, row := range gitlabRows {
		c.GraphQL(nil, row.op, row.doc, nil, nil)
	}
}

var gitlabRows = []struct{ op, doc string }{}
`, map[string]any{"gitlabRows": []struct{ op, doc string }{{"R", "query R { a }"}}})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"A githubA github", "A githubA github", "B githubB github", "R gitlabRows[0] gitlab"}; !slices.Equal(got, want) {
		t.Errorf("sends = %q, want %q", got, want)
	}
	for name, tc := range map[string]struct{ src, want string }{
		"a call's result":        {"func f() { c.GraphQL(nil, \"A\", build(), nil, nil) }\n\nfunc build() string { return \"\" }", "cannot trace build()"},
		"a non-constant var":     {"var githubQ = strings.Repeat(\"q\", 1)\n\nfunc f() { c.GraphQL(nil, \"A\", githubQ, nil, nil) }", "no constant"},
		"assigned apart":         {"func f() {\n\top := \"A\"\n\tdoc := githubA\n\tc.GraphQL(nil, op, doc, nil, nil)\n}", "assign them together"},
		"an unlisted table":      {"var rows = []struct{ op, doc string }{}\n\nfunc f() {\n\tfor _, r := range rows {\n\t\tc.GraphQL(nil, r.op, r.doc, nil, nil)\n\t}\n}", "list the table rows"},
		"a function value":       {"var hook = send\n\nfunc send(op, doc string) { c.GraphQL(nil, op, doc, nil, nil) }", "used other than called"},
		"an inline document":     {"func f() { c.GraphQL(nil, \"A\", `query A { a }`, nil, nil) }", "names no forge"},
		"a document of no forge": {"const readQ = `query A { a }`\n\nfunc f() { c.GraphQL(nil, \"A\", readQ, nil, nil) }", "names no forge"},
		"a GraphQL function":     {"func GraphQL() {}", "declares a GraphQL function"},
	} {
		if _, err := trace(tc.src, nil); err == nil {
			t.Errorf("%s: traced", name)
		} else if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
