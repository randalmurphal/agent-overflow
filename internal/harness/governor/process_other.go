//go:build !linux

package governor

type unsupportedProcesses struct{}

func (unsupportedProcesses) State(int) (ProcessState, error) { return ProcessState{}, ErrUnsupported }
func (unsupportedProcesses) RSS(int) (uint64, error)         { return 0, ErrUnsupported }
func defaultProcesses() ProcessReader                        { return unsupportedProcesses{} }
func defaultProcessMemory() ProcessMemoryReader              { return unsupportedProcesses{} }
