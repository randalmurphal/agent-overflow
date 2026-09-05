//go:build windows

package governor

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsTreeRSSRejectsReusedParentPID(t *testing.T) {
	rows := map[uint32]windowsProcessRow{
		10: {birth: 100, rss: 7},
		20: {parent: 10, birth: 110, rss: 7},
		30: {parent: 20, birth: 120, rss: 7},
		40: {parent: 10, birth: 50, rss: 1000},
		50: {parent: 40, birth: 150, rss: 1000},
	}
	rss, err := windowsTreeRSS(10, rows)
	if err != nil || rss != 21 {
		t.Fatalf("rss=%d err=%v; stale-parent subtree was counted", rss, err)
	}
	if _, err := windowsTreeRSS(99, rows); err == nil {
		t.Fatal("missing owner accepted")
	}
}

func TestWindowsProcessSnapshotReadsMemoryAndCreationTime(t *testing.T) {
	p := &windowsProcesses{}
	rows, err := p.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	self, ok := rows[uint32(os.Getpid())]
	if !ok || self.rss == 0 || self.birth == 0 {
		t.Fatalf("self snapshot=%+v", self)
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user); err != nil {
		t.Fatal(err)
	}
	if self.birth != int64(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime)) {
		t.Fatalf("snapshot birth %d differs from process handle %v", self.birth, created)
	}
	// The second sample reuses the buffer and still returns a complete tree.
	buffer := &p.buffer[0]
	bufferLength := len(p.buffer)
	rss, err := p.RSS(os.Getpid())
	if err != nil || rss == 0 {
		t.Fatalf("tree rss=%d err=%v", rss, err)
	}
	if len(p.buffer) == bufferLength && buffer != &p.buffer[0] {
		t.Fatal("steady-state snapshot replaced its buffer")
	}
}

func TestWindowsProcessSnapshotRejectsMalformedOffsets(t *testing.T) {
	size := int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))
	for _, step := range []uint32{1, uint32(size - 1), uint32(size + 1), uint32(size * 3)} {
		data := make([]byte, size*2)
		entry := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&data[0]))
		entry.NextEntryOffset = step
		if _, err := decodeWindowsProcessRows(data); err == nil {
			t.Fatalf("accepted next offset %d", step)
		}
	}
	if _, err := decodeWindowsProcessRows(make([]byte, size-1)); err == nil {
		t.Fatal("accepted truncated entry")
	}
}
