package gitapp

import "testing"

func TestFetchErrorMemoTransitions(t *testing.T) {
	var memo fetchErrorMemo
	const repo = "repo:/a/.git"
	if !memo.shouldLog(repo, "auth failed") || memo.shouldLog(repo, "auth failed") {
		t.Fatal("identical consecutive failure was not memoized")
	}
	if !memo.shouldLog(repo, "host unreachable") || memo.shouldLog(repo, "host unreachable") {
		t.Fatal("changed failure was not reported exactly once")
	}
	memo.clear(repo)
	if !memo.shouldLog(repo, "host unreachable") {
		t.Fatal("failure after recovery was not reported")
	}
	const other = "repo:/b/.git"
	if !memo.shouldLog(other, "host unreachable") {
		t.Fatal("one repository silenced another")
	}
	memo.retain(map[string]struct{}{repo: {}})
	if _, ok := memo.last[other]; ok {
		t.Fatal("retain kept a removed repository")
	}
	if !memo.shouldLog(other, "host unreachable") {
		t.Fatal("re-added repository did not report freshly")
	}
}
