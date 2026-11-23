package instanceinfo

// Process is one kernel observation of a process: its birth marker, as
// ProcessIdentity.StartTime holds it, its current parent, and whether it has
// exited (a zombie) and waits to be reaped. ReadProcess reads it on Linux
// and macOS.
type Process struct {
	Start  string
	PPID   int
	Exited bool
}
