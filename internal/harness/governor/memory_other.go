//go:build !linux

package governor

type unsupportedMemory struct{}

func (unsupportedMemory) AvailableMemory() (uint64, error) { return 0, ErrUnsupported }
func defaultMemory() MemoryReader                          { return unsupportedMemory{} }
