//go:build windows

package governor

import (
	"fmt"
	"strconv"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcesses struct {
	mu     sync.Mutex
	buffer []byte
}

func (*windowsProcesses) State(pid int) (ProcessState, error) {
	if pid <= 0 {
		return ProcessState{}, nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return ProcessState{}, nil
		}
		return ProcessState{}, fmt.Errorf("harness governor: open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return ProcessState{}, fmt.Errorf("harness governor: process %d creation time: %w", pid, err)
	}
	if exited.HighDateTime != 0 || exited.LowDateTime != 0 {
		return ProcessState{}, nil
	}
	return ProcessState{Alive: true, BirthID: strconv.FormatInt(created.Nanoseconds(), 10)}, nil
}

type windowsProcessRow struct {
	parent uint32
	birth  int64
	rss    uint64
}

func (p *windowsProcesses) RSS(pid int) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rows, err := p.snapshot()
	if err != nil {
		return 0, err
	}
	return windowsTreeRSS(uint32(pid), rows)
}

// One snapshot gives parent identity and memory without opening protected
// system processes. Reuse its buffer across watchdog samples.
func (p *windowsProcesses) snapshot() (map[uint32]windowsProcessRow, error) {
	if len(p.buffer) == 0 {
		p.buffer = make([]byte, 256*1024)
	}
	for attempt := 0; attempt < 5; attempt++ {
		var required uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&p.buffer[0]), uint32(len(p.buffer)), &required)
		if err == nil {
			if required > uint32(len(p.buffer)) {
				return nil, fmt.Errorf("harness governor: process snapshot exceeds its buffer")
			}
			return decodeWindowsProcessRows(p.buffer[:required])
		}
		if err != windows.STATUS_INFO_LENGTH_MISMATCH {
			return nil, fmt.Errorf("harness governor: process snapshot: %w", err)
		}
		size := max(uint64(len(p.buffer))*2, uint64(required)+uint64(required)/4)
		if size > 64*1024*1024 {
			return nil, fmt.Errorf("harness governor: process snapshot requires %d bytes", size)
		}
		p.buffer = make([]byte, int(size))
	}
	return nil, fmt.Errorf("harness governor: process snapshot kept growing during five attempts")
}

func decodeWindowsProcessRows(data []byte) (map[uint32]windowsProcessRow, error) {
	rows := make(map[uint32]windowsProcessRow)
	const size = int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))
	for offset := 0; ; {
		if len(data)-offset < size {
			return nil, fmt.Errorf("harness governor: truncated process snapshot at %d", offset)
		}
		entry := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&data[offset]))
		rows[uint32(entry.UniqueProcessID)] = windowsProcessRow{
			parent: uint32(entry.InheritedFromUniqueProcessID),
			birth:  entry.CreateTime,
			rss:    uint64(entry.WorkingSetSize),
		}
		if entry.NextEntryOffset == 0 {
			return rows, nil
		}
		step := uint64(entry.NextEntryOffset)
		if step < uint64(size) || step > uint64(len(data)-offset) || step%uint64(unsafe.Alignof(*entry)) != 0 {
			return nil, fmt.Errorf("harness governor: invalid process snapshot offset %d at %d", step, offset)
		}
		offset += int(step)
	}
}

// Parent PIDs outlive their process. Follow an edge only when that parent
// was born no later than the child; both identities come from one snapshot.
func windowsTreeRSS(root uint32, rows map[uint32]windowsProcessRow) (uint64, error) {
	if _, ok := rows[root]; !ok {
		return 0, fmt.Errorf("harness governor: process %d disappeared during tree sample", root)
	}
	children := make(map[uint32][]uint32)
	for child, row := range rows {
		children[row.parent] = append(children[row.parent], child)
	}
	queue := []uint32{root}
	seen := map[uint32]bool{root: true}
	var total uint64
	for i := 0; i < len(queue); i++ {
		current := rows[queue[i]]
		total += current.rss
		for _, child := range children[queue[i]] {
			if !seen[child] && rows[child].birth >= current.birth {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	return total, nil
}

func defaultProcesses() ProcessReader           { return &windowsProcesses{} }
func defaultProcessMemory() ProcessMemoryReader { return &windowsProcesses{} }
