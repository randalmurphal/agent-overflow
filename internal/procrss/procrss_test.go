package procrss

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeProc lays out a canned /proc-shaped tree: one directory per pid
// holding a `status` file in the kernel's own format.
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
