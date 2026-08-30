//go:build windows

package wsllauncher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func resolveCommand(opts LaunchOptions) CommandRunner {
	if opts.CommandRunner != nil {
		return opts.CommandRunner
	}
	return exec.CommandContext
}

// ListDistros runs `wsl.exe -l -v` and parses the output. Returns an
// empty slice and nil error when wsl.exe isn't on PATH or WSL itself
// isn't installed — the picker UI uses that as the cue to show install
// instructions.
//
// We use the documented `-l -v` (verbose list) form because the
// non-verbose form omits the State and Version columns.
func ListDistros(ctx context.Context) ([]Distro, error) {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		// wsl.exe not on PATH means WSL isn't installed; the picker
		// distinguishes this from "installed but no distros" by the
		// empty-slice return.
		return nil, nil
	}

	listCtx, cancel := context.WithTimeout(ctx, listDistrosTimeout)
	defer cancel()

	cmd := exec.CommandContext(listCtx, "wsl.exe", "-l", "-v")
	// Hide the wsl.exe console window when launched from a Wails GUI.
	// Without this, every list call flashes a black box on screen.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	return runListDistrosCmd(cmd)
}

// Launch spawns the WSL-side backend.
//
// Order:
//  1. Build the wsl.exe command line.
//  2. Wire stdout / stderr pipes BEFORE Start so we don't lose the
//     bootstrap line to a buffered FD.
//  3. Set CREATE_SUSPENDED via SysProcAttr so we can adopt the child
//     into the Job Object before any of its code runs — without this
//     a fast-failing child could exit before adopt() runs.
//  4. Start, adopt, resume.
//  5. Read the bootstrap line from stdout, discarding any pre-bootstrap
//     chatter to a debug log.
func Launch(ctx context.Context, opts LaunchOptions) (*Launcher, *Bootstrap, error) {
	if opts.Distro == "" {
		return nil, nil, errors.New("wsllauncher: LaunchOptions.Distro is required")
	}
	if opts.BinaryPath == "" {
		return nil, nil, errors.New("wsllauncher: LaunchOptions.BinaryPath is required")
	}
	prefix := opts.StdoutPrefix
	if prefix == "" {
		prefix = DefaultBootstrapPrefix
	}

	args := buildLaunchArgsWithMemoryLimit(opts.Distro, opts.BinaryPath, opts.ExtraArgs, opts.MemoryLimitBytes)

	runner := resolveCommand(opts)
	cmd := runner(ctx, "wsl.exe", args...)
	if len(opts.PassthroughEnv) > 0 {
		cmd.Env = AppendWSLENV(os.Environ(), opts.PassthroughEnv...)
	}
	// CREATE_SUSPENDED + HideWindow:
	//   - CREATE_SUSPENDED gives us a window to adopt the child into
	//     the Job Object before it runs a single instruction. Without
	//     it, a fast-failing child could exit before adopt() lands.
	//   - HideWindow stops a console flash on the Windows desktop. The
	//     WSL backend never wants console UI.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_SUSPENDED,
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("wire stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("wire stderr pipe: %w", err)
	}

	var platform platformLauncher
	if !opts.UseParentJob {
		platform, err = newPlatformLauncher()
		if err != nil {
			return nil, nil, fmt.Errorf("create job object: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		if platform != nil {
			_ = platform.close()
		}
		return nil, nil, fmt.Errorf("start wsl.exe: %w", err)
	}

	if platform != nil {
		if err := platform.adopt(cmd); err != nil {
			_ = cmd.Process.Kill()
			_ = platform.close()
			return nil, nil, fmt.Errorf("adopt child into job object: %w", err)
		}
	}

	// Resume the suspended primary thread. ResumeThread on the main
	// thread handle is the documented path; without this the child
	// stays frozen indefinitely.
	if err := resumePrimaryThread(cmd); err != nil {
		_ = cmd.Process.Kill()
		if platform != nil {
			_ = platform.close()
		}
		return nil, nil, fmt.Errorf("resume child thread: %w", err)
	}

	go drainStderr(stderr, func(line string) {
		log.Printf("wsllauncher: wsl[%s] stderr: %s", opts.Distro, line)
	})

	timeout := opts.BootstrapTimeout
	if timeout <= 0 {
		timeout = DefaultBootstrapTimeout
	}
	bsCtx, bsCancel := context.WithTimeout(ctx, timeout)
	defer bsCancel()
	bs, err := readBootstrapLine(bsCtx, stdout, prefix, func(line string) {
		log.Printf("wsllauncher: wsl[%s] stdout: %s", opts.Distro, line)
	})
	if err != nil {
		// readBootstrapLine returns on ctx cancellation but its scanner
		// goroutine stays parked inside an in-flight Read. Closing the
		// stdout pipe unparks it so the goroutine exits — without this,
		// every bootstrap timeout leaks a goroutine for the rest of the
		// launcher's lifetime.
		_ = stdout.Close()
		_ = cmd.Process.Kill()
		if platform != nil {
			_ = platform.close()
		}
		return nil, nil, err
	}

	return &Launcher{
		cmd:      cmd,
		platform: platform,
	}, &bs, nil
}

// InstallPayload copies the embedded Linux binary into the WSL distro
// at wslPath. hostPath is the absolute Windows-side path of the temp
// file we wrote the embedded payload to.
//
// Strategy: WSL automounts C: at /mnt/c, so we can rewrite the Windows
// path to its WSL form (C:\foo\bar -> /mnt/c/foo/bar) and use a single
// `cp` from inside the distro. This is faster and more reliable than
// piping the binary through wsl.exe stdin (which mangles binary on
// some Windows versions).
func InstallPayload(ctx context.Context, distro, hostPath, wslPath string) error {
	if distro == "" {
		return errors.New("wsllauncher: distro is required")
	}
	if hostPath == "" || wslPath == "" {
		return errors.New("wsllauncher: hostPath and wslPath are required")
	}

	wslHostPath, err := windowsToWSLPath(hostPath)
	if err != nil {
		return fmt.Errorf("translate host path: %w", err)
	}

	script := installPayloadScript(wslHostPath, wslPath, uniqueInstallTempPath(wslPath))

	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--", "/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install payload into %s:%s: %w (output: %s)", distro, wslPath, err, string(out))
	}
	return nil
}

func installPayloadScript(wslHostPath, wslPath, tmpPath string) string {
	return fmt.Sprintf(
		"set -e; mkdir -p %s; rm -f %s; trap %s EXIT; cp %s %s; chmod +x %s; mv -f %s %s; trap - EXIT",
		shellQuote(parentDir(wslPath)),
		shellQuote(tmpPath),
		shellQuote("rm -f "+tmpPath),
		shellQuote(wslHostPath),
		shellQuote(tmpPath),
		shellQuote(tmpPath),
		shellQuote(tmpPath),
		shellQuote(wslPath),
	)
}

func uniqueInstallTempPath(wslPath string) string {
	return fmt.Sprintf("%s.tmp.%d", wslPath, time.Now().UnixNano())
}

// windowsToWSLPath rewrites C:\foo\bar to /mnt/c/foo/bar. WSL2's
// default automount.root is /mnt; we don't try to read the user's
// /etc/wsl.conf override because the default is overwhelmingly the
// real-world configuration.
func windowsToWSLPath(p string) (string, error) {
	if len(p) < 2 || p[1] != ':' {
		return "", fmt.Errorf("not an absolute Windows path: %q", p)
	}
	drive := p[0]
	if drive >= 'A' && drive <= 'Z' {
		drive += 'a' - 'A'
	}
	rest := p[2:]
	// Forward slashes inside WSL.
	out := make([]byte, 0, len(rest)+8)
	out = append(out, "/mnt/"...)
	out = append(out, drive)
	for i := 0; i < len(rest); i++ {
		if rest[i] == '\\' {
			out = append(out, '/')
		} else {
			out = append(out, rest[i])
		}
	}
	return string(out), nil
}

// parentDir returns the parent of a Linux-style path. We can't use
// filepath.Dir because the Go Windows runtime uses backslashes there —
// we need POSIX semantics for the WSL-side path.
func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/"
			}
			return p[:i]
		}
	}
	return "."
}

// shellQuote wraps a path in single quotes and escapes embedded single
// quotes the POSIX way (' -> '"'"'). Sufficient for paths from the
// embedded payload (no special chars expected) but robust against the
// user installing into a directory with spaces.
func shellQuote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, "'\"'\"'"...)
		} else {
			out = append(out, s[i])
		}
	}
	out = append(out, '\'')
	return string(out)
}

// jobObjectLauncher pins a child process to a Win32 Job Object whose
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE flag tells the kernel to kill
// every assigned process when the last handle to the job is closed.
// Closing the launcher's parent .exe (the agent-overflow-windows
// binary) automatically closes its handles, so the WSL-side child
// never outlives the Windows GUI.
type jobObjectLauncher struct {
	handle windows.Handle
}

func newPlatformLauncher() (platformLauncher, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	// KILL_ON_JOB_CLOSE: kill every explicitly-assigned process (wsl.exe)
	// when the job handle goes away. This terminates the WSL session,
	// which cascades to all Linux processes in the distro. Without it
	// the WSL child keeps running, holding the listener port and the
	// SQLite write lock indefinitely.
	//
	// SILENT_BREAKAWAY_OK: child processes of job members automatically
	// do NOT inherit the job. Without this, Windows-side processes
	// spawned through WSL interop (browsers via rundll32, VS Code, etc.)
	// inherit the job from wsl.exe and get killed when the launcher
	// closes — even though they're standalone apps the user expects to
	// outlive us. The breakaway is safe because the WSL2 VM lifecycle is
	// managed by the Host Compute Service (HCS), not by our job —
	// killing wsl.exe signals HCS to tear down the session regardless
	// of whether helper processes broke away.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
				windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	return &jobObjectLauncher{handle: job}, nil
}

func (j *jobObjectLauncher) adopt(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("wsllauncher: cannot adopt nil process")
	}
	// cmd.Process.Pid is convertible to a Windows process handle by
	// reopening with PROCESS_SET_QUOTA | PROCESS_TERMINATE — those are
	// the rights AssignProcessToJobObject requires.
	procHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("OpenProcess pid=%d: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(procHandle)

	if err := windows.AssignProcessToJobObject(j.handle, procHandle); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

func (j *jobObjectLauncher) close() error {
	if j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	if err != nil {
		return fmt.Errorf("CloseHandle: %w", err)
	}
	return nil
}

// ntdll is the lazy handle to ntdll.dll. NtResumeProcess is the
// undocumented-but-stable kernel call Windows uses internally to
// resume every thread of a process started with CREATE_SUSPENDED.
// It's the same primitive a debugger uses on detach.
var (
	ntdll               = windows.NewLazySystemDLL("ntdll.dll")
	procNtResumeProcess = ntdll.NewProc("NtResumeProcess")
)

// resumePrimaryThread resumes the main thread of a child started with
// CREATE_SUSPENDED. We use ntdll.NtResumeProcess, the same syscall
// Windows uses for CREATE_SUSPENDED resume, instead of enumerating
// threads via Toolhelp32. The Toolhelp32 walk has two failure modes:
//
//   - Under a debugger or EDR with thread-injection, the snapshot
//     contains injected threads owned by the debugger / scanner. Calling
//     ResumeThread on those threads decrements their suspend counts and
//     can wake them prematurely.
//   - On a busy box, threads can be created between Thread32First and
//     Thread32Next; we'd resume some but not the new ones (or vice
//     versa).
//
// NtResumeProcess walks the kernel's thread list under a lock and only
// touches threads that belong to the target process — the precise
// behaviour we want. Callers must have PROCESS_SUSPEND_RESUME on the
// target's handle; OpenProcess below requests that explicitly.
func resumePrimaryThread(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("wsllauncher: nil process")
	}
	pid := uint32(cmd.Process.Pid)

	procHandle, err := windows.OpenProcess(
		windows.PROCESS_SUSPEND_RESUME,
		false,
		pid,
	)
	if err != nil {
		return fmt.Errorf("OpenProcess(SUSPEND_RESUME) pid=%d: %w", pid, err)
	}
	defer windows.CloseHandle(procHandle)

	if err := procNtResumeProcess.Find(); err != nil {
		return fmt.Errorf("locate ntdll.NtResumeProcess: %w", err)
	}
	// NtResumeProcess returns NTSTATUS in the eax register. Zero is
	// STATUS_SUCCESS; non-zero is an NTSTATUS error code which we wrap
	// for diagnostics. r1/r2 are unused for this syscall.
	r1, _, _ := procNtResumeProcess.Call(uintptr(procHandle))
	if r1 != 0 {
		return fmt.Errorf("NtResumeProcess returned NTSTATUS 0x%x", r1)
	}
	return nil
}
