//go:build darwin && cgo

package procrss

/*
#include <errno.h>
#include <libproc.h>
#include <stdint.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/sysctl.h>

// macOS does not expose application responsibility through a public header.
// This libSystem SPI is also used by Chromium's macOS process launcher and is
// available throughout Agent Overflow's supported macOS deployment range.
pid_t responsibility_get_pid_responsible_for_pid(pid_t pid);

static int ao_sysctl_all_procs(void *buf, size_t *size) {
	int mib[4] = { CTL_KERN, KERN_PROC, KERN_PROC_ALL, 0 };
	if (sysctl(mib, 4, buf, size, NULL, 0) == 0) return 0;
	return errno != 0 ? errno : EIO;
}

static int ao_proc_rss(int pid, uint64_t *rss) {
	struct proc_taskinfo info;
	memset(&info, 0, sizeof(info));
	int n = proc_pidinfo(pid, PROC_PIDTASKINFO, 0, &info, sizeof(info));
	if (n != sizeof(info)) return n == 0 && errno != 0 ? errno : EIO;
	*rss = info.pti_resident_size;
	return 0;
}

static int ao_proc_name(int pid, char *name, uint32_t size) {
	int n = proc_name(pid, name, size);
	if (n > 0) return n;
	return -(errno != 0 ? errno : ESRCH);
}
*/
import "C"

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinCommLimit          = 16
	darwinMaxRetainedBufSize = 8 * 1024 * 1024
)

// Persistent mutex-guarded scratch buffer for sysctl(KERN_PROC_ALL). Sampling
// runs on a 1s cadence; retaining the slab across GC cycles (unlike sync.Pool,
// which clears on GC) eliminates repeated ~1MB process-table allocations while
// capping retained memory at darwinMaxRetainedBufSize.
var (
	darwinScratchMu sync.Mutex
	darwinProcBuf   []byte
	darwinChildren  map[int][]int
	darwinChildPool []int
)

func acquireDarwinProcBuf() []byte {
	darwinScratchMu.Lock()
	buf := darwinProcBuf
	darwinProcBuf = nil
	darwinScratchMu.Unlock()
	if buf == nil {
		return make([]byte, 0, 512*1024)
	}
	return buf
}

func releaseDarwinProcBuf(buf []byte) {
	if cap(buf) > darwinMaxRetainedBufSize {
		return
	}
	darwinScratchMu.Lock()
	if darwinProcBuf == nil || cap(buf) > cap(darwinProcBuf) {
		darwinProcBuf = buf[:0]
	}
	darwinScratchMu.Unlock()
}

func readDarwinKinfoProcs(buf []byte) ([]byte, []unix.KinfoProc, error) {
	const entrySize = int(unsafe.Sizeof(unix.KinfoProc{}))
	for attempt := 0; attempt < 4; attempt++ {
		var need C.size_t
		if errno := C.ao_sysctl_all_procs(nil, &need); errno != 0 {
			return buf, nil, fmt.Errorf("procrss: size Darwin processes: %w", unix.Errno(errno))
		}
		if need == 0 {
			return buf, nil, nil
		}
		// Add 12.5% headroom for processes spawned between size probe and fill.
		target := int(need) + int(need)/8
		if cap(buf) < target {
			buf = make([]byte, target)
		} else {
			buf = buf[:cap(buf)]
		}
		got := C.size_t(len(buf))
		errno := C.ao_sysctl_all_procs(unsafe.Pointer(&buf[0]), &got)
		if unix.Errno(errno) == unix.ENOMEM {
			// Process table grew past buffer; grow and retry.
			buf = make([]byte, max(cap(buf)*2, int(got)+entrySize*64))
			continue
		}
		if errno != 0 {
			return buf, nil, fmt.Errorf("procrss: enumerate Darwin processes: %w", unix.Errno(errno))
		}
		count := int(got) / entrySize
		if count == 0 {
			return buf, nil, nil
		}
		entries := unsafe.Slice((*unix.KinfoProc)(unsafe.Pointer(&buf[0])), count)
		return buf, entries, nil
	}
	return buf, nil, fmt.Errorf("procrss: enumerate Darwin processes: %w", unix.ENOMEM)
}

type darwinProcess struct {
	pid       int
	ppid      int
	shortName string
}

type darwinProcessReader struct{}

func (darwinProcessReader) processes() ([]darwinProcess, error) {
	buf := acquireDarwinProcBuf()
	buf, entries, err := readDarwinKinfoProcs(buf)
	defer func() { releaseDarwinProcBuf(buf) }()
	if err != nil {
		return nil, err
	}
	rows := make([]darwinProcess, 0, len(entries))
	for i := range entries {
		entry := &entries[i]
		pid := int(entry.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		rows = append(rows, darwinProcess{
			pid:       pid,
			ppid:      int(entry.Eproc.Ppid),
			shortName: darwinComm(entry.Proc.P_comm[:]),
		})
	}
	return rows, nil
}

func (darwinProcessReader) responsible(pid int) (int, error) {
	responsible := int(C.responsibility_get_pid_responsible_for_pid(C.pid_t(pid)))
	if responsible <= 0 {
		return 0, fmt.Errorf("responsible pid for %d is unavailable", pid)
	}
	return responsible, nil
}

func (darwinProcessReader) process(pid int, fallbackName string) (Process, error) {
	var rss C.uint64_t
	if errno := C.ao_proc_rss(C.int(pid), &rss); errno != 0 {
		return Process{}, fmt.Errorf("rss for pid %d: %w", pid, unix.Errno(errno))
	}
	var name [1024]byte
	n := int(C.ao_proc_name(C.int(pid), (*C.char)(unsafe.Pointer(&name[0])), C.uint32_t(len(name))))
	if n > 0 {
		fallbackName = string(name[:n])
	}
	return Process{PID: pid, Name: fallbackName, RSSBytes: uint64(rss)}, nil
}

// Sample reads native Darwin process metadata. In addition to normal
// descendants, it recognizes WebKit XPC services assigned to the root's
// macOS responsible process, which launchd reparents away from the app.
func Sample(pid int, prefixes []string) (Tree, error) {
	if prefixes == nil {
		prefixes = DefaultWebviewPrefixes
	}
	return sampleDarwinNative(pid, func(name string) bool {
		return matchesAnyPrefix(name, prefixes)
	}, prefixes)
}

// SampleAll reports every ordinary descendant and every process assigned to
// an owned responsible process. This matches macOS's application ownership
// model and includes launchd-parented WebKit/Chrome helpers in totals.
func SampleAll(pid int) (Tree, error) {
	return sampleDarwinNative(pid, func(string) bool { return true }, nil)
}

// sampleDarwinNative implements the exact algorithm of sampleDarwin directly
// over the retained sysctl(KERN_PROC_ALL) kinfo_proc buffer, avoiding per-sample
// ~1MB kinfo allocations and per-process string conversions for unrelated host
// processes.
func sampleDarwinNative(pid int, match func(string) bool, prefixes []string) (Tree, error) {
	buf := acquireDarwinProcBuf()
	buf, entries, err := readDarwinKinfoProcs(buf)
	defer func() { releaseDarwinProcBuf(buf) }()
	if err != nil {
		return Tree{}, err
	}

	darwinScratchMu.Lock()
	childrenOf := darwinChildren
	darwinChildren = nil
	childPool := darwinChildPool
	darwinChildPool = nil
	darwinScratchMu.Unlock()
	if childrenOf == nil {
		childrenOf = make(map[int][]int, 256)
	} else {
		clear(childrenOf)
	}
	defer func() {
		clear(childrenOf)
		darwinScratchMu.Lock()
		if darwinChildren == nil {
			darwinChildren = childrenOf
		}
		if darwinChildPool == nil && cap(childPool) <= 4096 {
			darwinChildPool = childPool[:0]
		}
		darwinScratchMu.Unlock()
	}()

	rootIdx := -1
	for i := range entries {
		entry := &entries[i]
		p := int(entry.Proc.P_pid)
		if p <= 0 {
			continue
		}
		if p == pid {
			rootIdx = i
		}
		ppid := int(entry.Eproc.Ppid)
		childrenOf[ppid] = append(childrenOf[ppid], p)
	}
	if rootIdx < 0 {
		return Tree{}, fmt.Errorf("procrss: process %d disappeared during Darwin sample", pid)
	}
	reader := darwinProcessReader{}
	self, err := reader.process(pid, darwinComm(entries[rootIdx].Proc.P_comm[:]))
	if err != nil {
		return Tree{}, fmt.Errorf("procrss: read Darwin process %d: %w", pid, err)
	}

	descendants := make(map[int]bool)
	queue := append(childPool[:0], childrenOf[pid]...)
	for head := 0; head < len(queue); head++ {
		next := queue[head]
		if next == pid || descendants[next] {
			continue
		}
		descendants[next] = true
		queue = append(queue, childrenOf[next]...)
	}
	childPool = queue[:0]

	rootResponsible, responsibilityErr := reader.responsible(pid)
	responsibilityAvailable := responsibilityErr == nil && rootResponsible == pid
	responsibleOwners := map[int]bool{pid: true}
	if responsibilityAvailable {
		for descendant := range descendants {
			owner, readErr := reader.responsible(descendant)
			if readErr == nil && (owner == descendant || owner == pid || descendants[owner]) {
				responsibleOwners[owner] = true
			}
		}
	}

	candidates := make([]int, 0, len(descendants)+4)
	for i := range entries {
		entry := &entries[i]
		p := int(entry.Proc.P_pid)
		if p <= 0 || p == pid {
			continue
		}
		ordinaryDescendant := descendants[p]
		if !ordinaryDescendant && prefixes != nil && !couldMatchDarwinCommBytes(darwinCommBytes(entry.Proc.P_comm[:]), prefixes) {
			continue
		}
		sameResponsibility := false
		if responsibilityAvailable {
			owner, readErr := reader.responsible(p)
			sameResponsibility = readErr == nil && responsibleOwners[owner]
		}
		if ordinaryDescendant || sameResponsibility {
			candidates = append(candidates, i)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return entries[candidates[i]].Proc.P_pid < entries[candidates[j]].Proc.P_pid
	})

	tree := Tree{Self: self}
	for _, idx := range candidates {
		entry := &entries[idx]
		proc, readErr := reader.process(int(entry.Proc.P_pid), darwinComm(entry.Proc.P_comm[:]))
		if readErr != nil {
			continue // exited between process-table and libproc reads
		}
		if !match(proc.Name) {
			continue
		}
		tree.Children = append(tree.Children, proc)
		tree.ChildrenRSSBytes += proc.RSSBytes
	}
	return tree, nil
}

// Supported reports whether Sample can answer on this platform.
func Supported() bool { return true }

type darwinReader interface {
	processes() ([]darwinProcess, error)
	responsible(pid int) (int, error)
	process(pid int, fallbackName string) (Process, error)
}

func sampleDarwin(reader darwinReader, pid int, match func(string) bool, prefixes []string) (Tree, error) {
	rows, err := reader.processes()
	if err != nil {
		return Tree{}, err
	}
	byPID := make(map[int]darwinProcess, len(rows))
	childrenOf := make(map[int][]int, len(rows))
	for _, row := range rows {
		byPID[row.pid] = row
		childrenOf[row.ppid] = append(childrenOf[row.ppid], row.pid)
	}
	root, ok := byPID[pid]
	if !ok {
		return Tree{}, fmt.Errorf("procrss: process %d disappeared during Darwin sample", pid)
	}
	self, err := reader.process(pid, root.shortName)
	if err != nil {
		return Tree{}, fmt.Errorf("procrss: read Darwin process %d: %w", pid, err)
	}

	descendants := make(map[int]bool)
	queue := append([]int(nil), childrenOf[pid]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == pid || descendants[next] {
			continue
		}
		descendants[next] = true
		queue = append(queue, childrenOf[next]...)
	}

	// Only a self-responsible root owns a distinct macOS application set. A
	// raw executable launched from Terminal/another app inherits that app's
	// responsibility; charging its entire set here would count unrelated
	// processes. Windowed harness bundles explicitly disclaim at exec so their
	// root becomes self-responsible. If the API is unavailable, degrade to the
	// ordinary process tree rather than erase the RSS series.
	rootResponsible, responsibilityErr := reader.responsible(pid)
	responsibilityAvailable := responsibilityErr == nil && rootResponsible == pid
	responsibleOwners := map[int]bool{pid: true}
	if responsibilityAvailable {
		for descendant := range descendants {
			owner, readErr := reader.responsible(descendant)
			if readErr == nil && (owner == descendant || owner == pid || descendants[owner]) {
				responsibleOwners[owner] = true
			}
		}
	}
	candidates := make([]darwinProcess, 0, len(descendants)+4)
	for _, row := range rows {
		if row.pid == pid {
			continue
		}
		ordinaryDescendant := descendants[row.pid]
		if !ordinaryDescendant && prefixes != nil && !couldMatchDarwinComm(row.shortName, prefixes) {
			continue
		}
		sameResponsibility := false
		if responsibilityAvailable {
			owner, readErr := reader.responsible(row.pid)
			sameResponsibility = readErr == nil && responsibleOwners[owner]
		}
		if ordinaryDescendant || sameResponsibility {
			candidates = append(candidates, row)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].pid < candidates[j].pid })

	tree := Tree{Self: self}
	for _, row := range candidates {
		proc, readErr := reader.process(row.pid, row.shortName)
		if readErr != nil {
			continue // exited between process-table and libproc reads
		}
		if !match(proc.Name) {
			continue
		}
		tree.Children = append(tree.Children, proc)
		tree.ChildrenRSSBytes += proc.RSSBytes
	}
	return tree, nil
}

func couldMatchDarwinComm(short string, prefixes []string) bool {
	if matchesAnyPrefix(short, prefixes) {
		return true
	}
	// p_comm is capped at 16 bytes. A more-specific caller prefix can begin
	// with the entire truncated comm, in which case proc_name must decide.
	if len(short) < darwinCommLimit {
		return false
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(prefix, short) {
			return true
		}
	}
	return false
}

func darwinCommBytes(raw []byte) []byte {
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		return raw[:i]
	}
	return raw
}

func couldMatchDarwinCommBytes(short []byte, prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		if len(short) >= len(prefix) && string(short[:len(prefix)]) == prefix {
			return true
		}
	}
	if len(short) < darwinCommLimit {
		return false
	}
	for _, prefix := range prefixes {
		if len(prefix) >= len(short) && prefix[:len(short)] == string(short) {
			return true
		}
	}
	return false
}

func darwinComm(raw []byte) string {
	return string(darwinCommBytes(raw))
}
