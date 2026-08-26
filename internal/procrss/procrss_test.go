package procrss

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeProc lays out a canned /proc-shaped tree: one directory per pid
// holding the two files the walk reads — `status` (name, VmRSS) and
// `stat` (comm, ppid) — both in the kernel's own format. Both are written
// from the same status body, so a fixture cannot accidentally describe a
// process whose two files disagree.
func writeProc(t *testing.T, entries map[int]string, extras map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, status := range entries {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %d: %v", pid, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatalf("write status %d: %v", pid, err)
		}
		name, _, ppid := ParseStatus([]byte(status))
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statLine(pid, name, ppid)), 0o644); err != nil {
			t.Fatalf("write stat %d: %v", pid, err)
		}
	}
	for name, body := range extras {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write extra %s: %v", name, err)
		}
	}
	return root
}

func status(name string, ppid int, rssKB string) string {
	body := "Name:\t" + name + "\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t1\nPPid:\t" +
		strconv.Itoa(ppid) + "\nThreads:\t7\n"
	if rssKB != "" {
		body += "VmSize:\t 1000000 kB\nVmRSS:\t " + rssKB + " kB\nRssAnon:\t 1 kB\n"
	}
	return body
}

// statLine is /proc/<pid>/stat's shape: pid, (comm), state, ppid, then a
// long tail of numbers the walk never looks at.
func statLine(pid int, comm string, ppid int) string {
	return strconv.Itoa(pid) + " (" + comm + ") S " + strconv.Itoa(ppid) +
		" 1 1 0 -1 4194304 1234 0 0 0 5 3 0 0 20 0 7 0 98765 123456789 4096\n"
}

func TestParseStatusReadsNamePpidAndRSS(t *testing.T) {
	name, rssKB, ppid := ParseStatus([]byte(status("agent-overflow", 42, "51200")))
	if name != "agent-overflow" {
		t.Errorf("name = %q, want agent-overflow", name)
	}
	if ppid != 42 {
		t.Errorf("ppid = %d, want 42", ppid)
	}
	if rssKB != 51200 {
		t.Errorf("rssKB = %d, want 51200", rssKB)
	}
}

// A kernel thread has no VmRSS line at all. Reading zero (rather than
// failing) is what keeps the walk from dropping a whole sample because one
// unrelated pid on the host is a kthread.
func TestParseStatusToleratesMissingVmRSS(t *testing.T) {
	name, rssKB, ppid := ParseStatus([]byte(status("kworker/3:1", 2, "")))
	if name != "kworker/3:1" || ppid != 2 {
		t.Fatalf("got name=%q ppid=%d", name, ppid)
	}
	if rssKB != 0 {
		t.Errorf("rssKB = %d, want 0", rssKB)
	}
}

func TestParseStatReadsCommAndPpid(t *testing.T) {
	name, ppid, ok := ParseStat([]byte(statLine(101, "WebKitWebProce", 100)))
	if !ok || name != "WebKitWebProce" || ppid != 100 {
		t.Fatalf("ParseStat = (%q, %d, %v)", name, ppid, ok)
	}
}

// comm is whatever the process called itself, parentheses and spaces
// included. Splitting the line on whitespace would read the state letter
// as the ppid here, which is exactly the misparse that would silently
// disconnect a renderer from its parent.
func TestParseStatHandlesACommWithSpacesAndParens(t *testing.T) {
	name, ppid, ok := ParseStat([]byte(statLine(7, "(Web Content) 1", 100)))
	if !ok {
		t.Fatal("ParseStat refused a legal comm")
	}
	if name != "(Web Content) 1" {
		t.Errorf("comm = %q", name)
	}
	if ppid != 100 {
		t.Errorf("ppid = %d, want 100", ppid)
	}
}

func TestParseStatRefusesATruncatedLine(t *testing.T) {
	for _, line := range []string{"", "123", "123 (comm", "123 (comm) S"} {
		if _, _, ok := ParseStat([]byte(line)); ok {
			t.Errorf("ParseStat accepted %q", line)
		}
	}
}

func TestSampleRootCollectsMatchingDescendants(t *testing.T) {
	root := writeProc(t, map[int]string{
		1:   status("systemd", 0, "9000"),
		100: status("agent-overflow", 1, "51200"),
		// The kernel truncates Name at 15 chars — the renderer is
		// "WebKitWebProce", never "WebKitWebProcess". Prefix matching is
		// the whole point of this case.
		101: status("WebKitWebProce", 100, "204800"),
		102: status("WebKitNetworkP", 100, "10240"),
		// A grandchild: re-parented helpers must still be found.
		103: status("WebKitGPUProce", 101, "20480"),
		// A non-webview child of ours, and a webview belonging to someone
		// else. Neither may land in the sample.
		104: status("git", 100, "4096"),
		200: status("WebKitWebProce", 1, "999999"),
	}, map[string]string{"meminfo": "MemTotal: 1 kB\n", "self": ""})

	tree, err := SampleRoot(root, 100, DefaultWebviewPrefixes)
	if err != nil {
		t.Fatalf("SampleRoot: %v", err)
	}
	if tree.Self.Name != "agent-overflow" || tree.Self.RSSBytes != 51200*1024 {
		t.Fatalf("self = %+v", tree.Self)
	}
	got := map[int]uint64{}
	for _, child := range tree.Children {
		got[child.PID] = child.RSSBytes
	}
	want := map[int]uint64{101: 204800 * 1024, 102: 10240 * 1024, 103: 20480 * 1024}
	if len(got) != len(want) {
		t.Fatalf("children = %+v, want pids %v", tree.Children, []int{101, 102, 103})
	}
	for pid, bytes := range want {
		if got[pid] != bytes {
			t.Errorf("child %d rss = %d, want %d", pid, got[pid], bytes)
		}
	}
	if tree.ChildrenRSSBytes != (204800+10240+20480)*1024 {
		t.Errorf("ChildrenRSSBytes = %d", tree.ChildrenRSSBytes)
	}
	if tree.TotalRSSBytes() != (51200+204800+10240+20480)*1024 {
		t.Errorf("TotalRSSBytes = %d", tree.TotalRSSBytes())
	}
}

// TestSampleAllRootTakesEveryDescendant pins the difference between the two
// walks on one canned tree: the prefix walk answers "how big is the
// renderer", SampleAllRoot answers "how big is this instance", so the
// non-webview child counts and the foreign webview still does not.
func TestSampleAllRootTakesEveryDescendant(t *testing.T) {
	root := writeProc(t, map[int]string{
		1:   status("systemd", 0, "9000"),
		100: status("agent-overflow", 1, "51200"),
		101: status("WebKitWebProce", 100, "204800"),
		104: status("ao-mockprovide", 100, "4096"),
		105: status("git", 104, "2048"),
		200: status("WebKitWebProce", 1, "999999"),
	}, nil)

	tree, err := SampleAllRoot(root, 100)
	if err != nil {
		t.Fatalf("SampleAllRoot: %v", err)
	}
	got := map[int]uint64{}
	for _, child := range tree.Children {
		got[child.PID] = child.RSSBytes
	}
	want := map[int]uint64{101: 204800 * 1024, 104: 4096 * 1024, 105: 2048 * 1024}
	if len(got) != len(want) {
		t.Fatalf("children = %+v, want pids %v", tree.Children, []int{101, 104, 105})
	}
	for pid, bytes := range want {
		if got[pid] != bytes {
			t.Errorf("child %d rss = %d, want %d", pid, got[pid], bytes)
		}
	}
	if tree.TotalRSSBytes() != (51200+204800+4096+2048)*1024 {
		t.Errorf("TotalRSSBytes = %d", tree.TotalRSSBytes())
	}
}

// The parent map comes from `stat` alone, so a process that is none of
// our business costs one cheap read and never an expensive `status` one.
// Proving that from the outside means proving the walk still answers when
// the foreign processes have no `status` file at all — which is also what
// happens for real when a stranger's pid exits mid-walk.
func TestSampleRootDoesNotNeedStatusForProcessesItDoesNotReport(t *testing.T) {
	root := writeProc(t, map[int]string{
		100: status("agent-overflow", 1, "51200"),
		101: status("WebKitWebProce", 100, "204800"),
	}, nil)
	// A hundred strangers with a stat file and nothing else.
	for pid := 1000; pid < 1100; pid++ {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %d: %v", pid, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statLine(pid, "stranger", 1)), 0o644); err != nil {
			t.Fatalf("write stat %d: %v", pid, err)
		}
	}
	tree, err := SampleRoot(root, 100, DefaultWebviewPrefixes)
	if err != nil {
		t.Fatalf("SampleRoot: %v", err)
	}
	if len(tree.Children) != 1 || tree.Children[0].PID != 101 {
		t.Fatalf("children = %+v, want only pid 101", tree.Children)
	}
}

// A descendant that exits BETWEEN the two passes has a parent entry and
// no RSS. Skipping it must not cost its own children, whose place in the
// tree came from the first pass and is still true.
func TestSampleRootKeepsGrandchildrenOfADescendantThatExitedMidWalk(t *testing.T) {
	root := writeProc(t, map[int]string{
		100: status("agent-overflow", 1, "51200"),
		101: status("WebKitWebProce", 100, "204800"),
		102: status("WebKitGPUProce", 101, "20480"),
	}, nil)
	if err := os.Remove(filepath.Join(root, "101", "status")); err != nil {
		t.Fatalf("remove status: %v", err)
	}
	tree, err := SampleRoot(root, 100, DefaultWebviewPrefixes)
	if err != nil {
		t.Fatalf("SampleRoot: %v", err)
	}
	if len(tree.Children) != 1 || tree.Children[0].PID != 102 {
		t.Fatalf("children = %+v, want the surviving grandchild 102", tree.Children)
	}
	if tree.ChildrenRSSBytes != 20480*1024 {
		t.Errorf("ChildrenRSSBytes = %d", tree.ChildrenRSSBytes)
	}
}

func TestSampleRootFailsWhenTheNamedProcessIsGone(t *testing.T) {
	root := writeProc(t, map[int]string{1: status("systemd", 0, "9000")}, nil)
	if _, err := SampleRoot(root, 4242, DefaultWebviewPrefixes); err == nil {
		t.Fatal("SampleRoot must fail when its own pid has no status file")
	}
}

// A descendant that exits between the readdir and the read is skipped, not
// fatal: the tree is racy by construction and a perf series must not
// develop holes because a renderer restarted.
func TestSampleRootSkipsAnUnreadableDescendant(t *testing.T) {
	root := writeProc(t, map[int]string{
		100: status("agent-overflow", 1, "51200"),
		101: status("WebKitWebProce", 100, "204800"),
	}, nil)
	if err := os.MkdirAll(filepath.Join(root, "102"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	} // pid 102 has no status file at all
	tree, err := SampleRoot(root, 100, DefaultWebviewPrefixes)
	if err != nil {
		t.Fatalf("SampleRoot: %v", err)
	}
	if len(tree.Children) != 1 || tree.Children[0].PID != 101 {
		t.Fatalf("children = %+v, want only pid 101", tree.Children)
	}
}
