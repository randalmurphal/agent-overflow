//go:build windows

package governor

import (
	"fmt"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcesses struct{}

func (windowsProcesses) State(pid int) (ProcessState, error) {
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
	return ProcessState{Alive: true, BirthID: strconv.FormatInt(created.Nanoseconds(), 10)}, nil
}

type windowsProcessRow struct{ parent uint32 }

func (windowsProcesses) RSS(pid int) (uint64, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, fmt.Errorf("harness governor: process snapshot: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	rows := make(map[uint32]windowsProcessRow)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, fmt.Errorf("harness governor: process snapshot first: %w", err)
	}
	for {
		rows[entry.ProcessID] = windowsProcessRow{parent: entry.ParentProcessID}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return 0, fmt.Errorf("harness governor: process snapshot next: %w", err)
		}
	}
	root := uint32(pid)
	if _, ok := rows[root]; !ok {
		return 0, fmt.Errorf("harness governor: process %d disappeared during tree sample", pid)
	}
	var total uint64
	queue := []uint32{root}
	seen := map[uint32]bool{root: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		rss, err := windowsProcessRSS(current)
		if err != nil {
			return 0, err
		}
		total += rss
		for child, row := range rows {
			if row.parent == current && !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	return total, nil
}

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

func windowsProcessRSS(pid uint32) (uint64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, fmt.Errorf("harness governor: open process %d memory: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	counters := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	r, _, callErr := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(unsafe.Sizeof(counters)))
	if r == 0 {
		return 0, fmt.Errorf("harness governor: GetProcessMemoryInfo(%d): %w", pid, callErr)
	}
	return uint64(counters.WorkingSetSize), nil
}

func defaultProcesses() ProcessReader           { return windowsProcesses{} }
func defaultProcessMemory() ProcessMemoryReader { return windowsProcesses{} }
