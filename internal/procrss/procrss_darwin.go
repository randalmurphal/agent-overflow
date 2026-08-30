//go:build darwin && cgo

package procrss

/*
#include <errno.h>
#include <libproc.h>
#include <stdint.h>
#include <string.h>
#include <sys/proc_info.h>

// macOS does not expose application responsibility through a public header.
// This libSystem SPI is also used by Chromium's macOS process launcher and is
// available throughout Agent Overflow's supported macOS deployment range.
pid_t responsibility_get_pid_responsible_for_pid(pid_t pid);

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
	"fmt"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const darwinCommLimit = 16

type darwinProcess struct {
	pid       int
	ppid      int
	shortName string
}

type darwinProcessReader struct{}

func (darwinProcessReader) processes() ([]darwinProcess, error) {
	entries, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("procrss: enumerate Darwin processes: %w", err)
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
	name := make([]byte, 1024)
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
	return sampleDarwin(darwinProcessReader{}, pid, func(name string) bool {
		return matchesAnyPrefix(name, prefixes)
	}, prefixes)
}

// SampleAll reports every ordinary descendant and every process assigned to
// an owned responsible process. This matches macOS's application ownership
// model and includes launchd-parented WebKit/Chrome helpers in totals.
func SampleAll(pid int) (Tree, error) {
	return sampleDarwin(darwinProcessReader{}, pid, func(string) bool { return true }, nil)
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

func darwinComm(raw []byte) string {
	buf := make([]byte, 0, len(raw))
	for _, value := range raw {
		if value == 0 {
			break
		}
		buf = append(buf, value)
	}
	return string(buf)
}
