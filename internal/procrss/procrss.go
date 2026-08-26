package procrss

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrUnsupported is returned by Sample on a platform with no /proc.
var ErrUnsupported = errors.New("procrss: /proc is not available on this platform")

// DefaultWebviewPrefixes matches every WebKitGTK helper process a windowed
// harness spawns: WebKitWebProcess (the renderer), WebKitNetworkProcess,
// WebKitGPUProcess.
//
// Prefixes, not exact names, and that is load-bearing: the kernel caps
// /proc/<pid>/status's `Name:` at 15 characters, so the renderer reads back
// as "WebKitWebProce" — an exact-name match would silently find nothing,
// which is the failure mode this whole package exists to avoid.
var DefaultWebviewPrefixes = []string{"WebKit"}

// Process is one process's identity and resident size.
type Process struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	// RSSBytes is VmRSS. Zero for a kernel thread or a process whose
	// status file carried no VmRSS line (it had already exited).
	RSSBytes uint64 `json:"rssBytes"`
}

// Tree is one sample: the named process plus the matching descendants it
// owns.
type Tree struct {
	Self     Process   `json:"self"`
	Children []Process `json:"children,omitempty"`
	// ChildrenRSSBytes is the sum over Children, precomputed so a consumer
	// folding a series does not re-walk the slice per sample.
	ChildrenRSSBytes uint64 `json:"childrenRssBytes"`
}

// TotalRSSBytes is self plus every matched descendant.
func (t Tree) TotalRSSBytes() uint64 { return t.Self.RSSBytes + t.ChildrenRSSBytes }

// SampleRoot is Sample with the procfs root injected, which is what makes
// the walk testable against canned testdata. `prefixes` selects which
// DESCENDANTS are reported; self is always reported.
//
// A descendant that exits mid-walk is skipped rather than failing the
// sample: process trees are racy by nature and a perf series must not
// develop holes because a renderer restarted.
func SampleRoot(root string, pid int, prefixes []string) (Tree, error) {
	return sampleRoot(root, pid, func(name string) bool { return matchesAnyPrefix(name, prefixes) })
}

// SampleAllRoot reports EVERY descendant, whatever it is named.
//
// Two questions, two walks: a perf run asks "how big did the renderer get"
// and wants the webview processes alone, while a health rollup asks "how
// much memory is this instance holding" and a provider mock or a spawned
// git is as much part of the answer as the renderer is. An empty prefix
// cannot express the second question, because a prefix that matches
// everything would also have to match a process whose status file carried
// no name at all.
func SampleAllRoot(root string, pid int) (Tree, error) {
	return sampleRoot(root, pid, func(string) bool { return true })
}

func sampleRoot(root string, pid int, match func(name string) bool) (Tree, error) {
	self, err := readProcess(root, pid)
	if err != nil {
		return Tree{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Tree{}, err
	}

	type record struct {
		proc Process
		ppid int
	}
	records := make(map[int]record, len(entries))
	childrenOf := make(map[int][]int, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		other, convErr := strconv.Atoi(entry.Name())
		if convErr != nil {
			continue // /proc/self, /proc/meminfo, …
		}
		proc, ppid, readErr := readProcessWithParent(root, other)
		if readErr != nil {
			continue // exited between the readdir and the read
		}
		records[other] = record{proc: proc, ppid: ppid}
		childrenOf[ppid] = append(childrenOf[ppid], other)
	}

	tree := Tree{Self: self}
	// Breadth-first over descendants, so a renderer re-parented under a
	// helper is still found. Bounded by the number of live pids, and a
	// visited set keeps a cyclic ppid (impossible in practice, cheap to
	// rule out) from looping forever.
	visited := map[int]bool{pid: true}
	queue := append([]int(nil), childrenOf[pid]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if visited[next] {
			continue
		}
		visited[next] = true
		queue = append(queue, childrenOf[next]...)
		rec, ok := records[next]
		if !ok || !match(rec.proc.Name) {
			continue
		}
		tree.Children = append(tree.Children, rec.proc)
		tree.ChildrenRSSBytes += rec.proc.RSSBytes
	}
	return tree, nil
}

func matchesAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func readProcess(root string, pid int) (Process, error) {
	proc, _, err := readProcessWithParent(root, pid)
	return proc, err
}

func readProcessWithParent(root string, pid int) (Process, int, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "status"))
	if err != nil {
		return Process{}, 0, err
	}
	name, rssKB, ppid := ParseStatus(data)
	return Process{PID: pid, Name: name, RSSBytes: rssKB * 1024}, ppid, nil
}

// ParseStatus pulls the three fields a sample needs out of a
// /proc/<pid>/status body: `Name`, `VmRSS` (kB), and `PPid`. Exported so the
// parse is unit-tested directly against real-world status text, including
// the shapes that make a naive parser wrong — a kernel thread with no VmRSS
// line at all, and a `Name` the kernel truncated to 15 characters.
func ParseStatus(data []byte) (name string, rssKB uint64, ppid int) {
	for len(data) > 0 {
		var line []byte
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			line, data = data[:idx], data[idx+1:]
		} else {
			line, data = data, nil
		}
		key, value, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		switch string(key) {
		case "Name":
			name = string(bytes.TrimSpace(value))
		case "PPid":
			ppid, _ = strconv.Atoi(string(bytes.TrimSpace(value)))
		case "VmRSS":
			// "  123456 kB" — the unit is always kB, but read the number
			// only so a future unit change degrades to a wrong scale
			// rather than a parse failure.
			fields := strings.Fields(string(value))
			if len(fields) > 0 {
				rssKB, _ = strconv.ParseUint(fields[0], 10, 64)
			}
		}
	}
	return name, rssKB, ppid
}
