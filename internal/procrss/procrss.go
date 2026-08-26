package procrss

import (
	"bytes"
	"errors"
	"fmt"
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

// sampleRoot walks the tree in TWO passes, and the split is the whole
// performance story of this package. The parent map has to cover every
// pid on the host — a re-parented renderer is only findable through
// processes that are none of our business — but `status` is a long,
// many-line file the kernel formats per read, and a perf run walks this
// once a second. So pass one reads `stat` (one line, comm and ppid) for
// every pid, and pass two reads `status` only for the handful of
// processes that turned out to be ours: on a busy host that is hundreds
// of cheap reads instead of hundreds of expensive ones.
//
// RSS still comes from `status`'s VmRSS, deliberately: `stat`'s field 24
// is a page count over a slightly different set of pages, and switching
// would silently move every number this package has ever reported.
func sampleRoot(root string, pid int, match func(name string) bool) (Tree, error) {
	self, err := readProcess(root, pid)
	if err != nil {
		return Tree{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Tree{}, err
	}

	names := make(map[int]string, len(entries))
	childrenOf := make(map[int][]int, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		other, convErr := strconv.Atoi(entry.Name())
		if convErr != nil {
			continue // /proc/self, /proc/meminfo, …
		}
		name, ppid, readErr := readStat(root, other)
		if readErr != nil {
			continue // exited between the readdir and the read
		}
		names[other] = name
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
		name, ok := names[next]
		if !ok || !match(name) {
			continue
		}
		proc, readErr := readProcess(root, next)
		if readErr != nil {
			continue // exited between the two passes
		}
		tree.Children = append(tree.Children, proc)
		tree.ChildrenRSSBytes += proc.RSSBytes
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
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "status"))
	if err != nil {
		return Process{}, err
	}
	name, rssKB, _ := ParseStatus(data)
	return Process{PID: pid, Name: name, RSSBytes: rssKB * 1024}, nil
}

func readStat(root string, pid int) (name string, ppid int, err error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, err
	}
	name, ppid, ok := ParseStat(data)
	if !ok {
		return "", 0, fmt.Errorf("procrss: /proc/%d/stat is not in the expected form", pid)
	}
	return name, ppid, nil
}

// ParseStat pulls the two fields the parent map needs out of a
// /proc/<pid>/stat line: `comm` (field 2) and `ppid` (field 4). Exported
// so the parse is unit-tested directly.
//
// Splitting on whitespace is WRONG here and that is the whole reason this
// function exists: comm is the executable name in parentheses and may
// contain spaces and parentheses of its own — a WebKit helper renamed to
// "(Web Content) 1" would shift every later field. The scan takes the
// LAST ')' in the line instead, which no later field can contain, and
// counts fields from there.
func ParseStat(data []byte) (name string, ppid int, ok bool) {
	open := bytes.IndexByte(data, '(')
	shut := bytes.LastIndexByte(data, ')')
	if open < 0 || shut < open {
		return "", 0, false
	}
	name = string(data[open+1 : shut])
	// After comm come state (field 3) and ppid (field 4).
	rest := bytes.Fields(data[shut+1:])
	if len(rest) < 2 {
		return "", 0, false
	}
	ppid, err := strconv.Atoi(string(rest[1]))
	if err != nil {
		return "", 0, false
	}
	return name, ppid, true
}

// ParseStatus pulls three fields out of a /proc/<pid>/status body:
// `Name`, `VmRSS` (kB), and `PPid`. The walk uses the first two (the
// parent map comes from the cheaper `stat` file); PPid is returned
// because it is the same fact from the authoritative file, and a caller
// that already paid for the read should not have to parse a second one.
// Exported so the parse is unit-tested directly against real-world status
// text, including the shapes that make a naive parser wrong — a kernel
// thread with no VmRSS line at all, and a `Name` the kernel truncated to
// 15 characters.
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
