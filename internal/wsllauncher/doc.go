// Package wsllauncher detects WSL distributions and spawns the
// agent-overflow Linux backend inside one, pinning the child process
// to a Win32 Job Object so it cannot outlive the Windows-side
// launcher. Used only by cmd/agent-overflow-windows.
package wsllauncher
