//go:build !unix

package app

// addUnixCases adds nothing where there is no mode 000 or FIFO.
func addUnixCases(map[string]homeBreak) {}
