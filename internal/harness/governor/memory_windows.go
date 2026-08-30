//go:build windows

package governor

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsMemoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

type windowsMemory struct{}

func (windowsMemory) AvailableMemory() (uint64, error) {
	status := windowsMemoryStatusEx{Length: uint32(unsafe.Sizeof(windowsMemoryStatusEx{}))}
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		if err == nil {
			err = errors.New("call returned false")
		}
		return 0, fmt.Errorf("harness governor: GlobalMemoryStatusEx: %w", err)
	}
	return status.AvailPhys, nil
}

func defaultMemory() MemoryReader { return windowsMemory{} }
