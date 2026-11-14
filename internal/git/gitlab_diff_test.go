package git

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// A merge request that renames z.go to a.go and adds a new z.go has two
// entries naming z.go: the new file's own, and the rename's old path. A
// lookup of z.go takes the new file in either order, with the two on
// different pages, and a lookup of a path no entry has as its new path
// takes the rename's old path once every page is read.
func TestGitLabDiffLookupPrefersTheNewPath(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	renamed := `{"old_path":"z.go","new_path":"a.go","renamed_file":true,"diff":` + strconv.Quote("@@ -1,2 +1,2 @@\n k\n-old\n+new\n") + `}`
	added := `{"old_path":"z.go","new_path":"z.go","new_file":true,"diff":` + strconv.Quote("@@ -0,0 +1,1 @@\n+fresh\n") + `}`
	gone := `{"old_path":"y.go","new_path":"x.go","renamed_file":true,"diff":` + strconv.Quote("@@ -1 +1 @@\n-a\n+b\n") + `}`
	for name, pages := range map[string][]string{
		"rename first": {renamed, added + "," + gone},
		"new first":    {added, renamed + "," + gone},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				path, rawQuery, _ := strings.Cut(call.Path, "?")
				if call.Method != http.MethodGet || path != mr+"/diffs" {
					return forgeUnexpected(t, call)
				}
				query, _ := url.ParseQuery(rawQuery)
				page := 1
				if raw := query.Get("page"); raw != "" {
					page, _ = strconv.Atoi(raw)
				}
				answer := forgeAPIAnswer{Header: http.Header{}, Body: "[" + pages[page-1] + "]"}
				if page < len(pages) {
					answer.Header.Set("X-Next-Page", strconv.Itoa(page+1))
				}
				return answer
			})
			client, err := core.gitlabAPI(gitlabTestRef(7).Host)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range [][]string{{"z.go"}, {"z.go", "y.go"}} {
				files, err := readGitLabDiffFiles(t.Context(), client, gitlabTestRef(7), want)
				if err != nil {
					t.Fatal(err)
				}
				if got := files["z.go"]; got.newPath != "z.go" || got.oldPath != "z.go" {
					t.Fatalf("%v: z.go = %s -> %s, want the new file (requests %v)", want, got.oldPath, got.newPath, calls.ops())
				}
				if got, ok := files["y.go"]; len(want) == 2 && (got.newPath != "x.go" || got.oldPath != "y.go") || len(want) == 1 && ok {
					t.Fatalf("%v: y.go = %+v", want, got)
				}
			}
		})
	}
}
