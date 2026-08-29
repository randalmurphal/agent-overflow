//go:build linux

package containment

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type linuxGroup struct {
	file *os.File
	path string
	mu   sync.Mutex
	done bool
}

// Prepare creates a private cgroup v2 below the caller's delegated cgroup.
// It fails closed when the host does not delegate a writable memory cgroup.
// The limit is installed before exec via SysProcAttr.UseCgroupFD, avoiding a
// post-Start window where a runaway child could escape the budget.
func Prepare(limit uint64) (Group, error) {
	if limit == 0 {
		return nil, errors.New("harness containment: memory limit must be positive")
	}
	parent, err := currentCgroupPath()
	if err != nil {
		return nil, err
	}
	if err := pruneOrphanGroups(parent); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("agent-overflow-%d-%d", os.Getpid(), time.Now().UnixNano())
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, fmt.Errorf("create cgroup %s: %w", path, err)
	}
	remove := func() { _ = os.Remove(path) }
	if err := writeCgroupValue(filepath.Join(path, "memory.max"), strconv.FormatUint(limit, 10)); err != nil {
		remove()
		return nil, fmt.Errorf("set memory.max: %w", err)
	}
	// Swap is part of the aggregate memory budget. If this controller is not
	// available, keeping swap unlimited defeats the OOM protection promise.
	if err := writeCgroupValue(filepath.Join(path, "memory.swap.max"), "0"); err != nil {
		remove()
		return nil, fmt.Errorf("set memory.swap.max: %w", err)
	}
	if err := writeCgroupValue(filepath.Join(path, "memory.oom.group"), "1"); err != nil {
		remove()
		return nil, fmt.Errorf("set memory.oom.group: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		remove()
		return nil, fmt.Errorf("open cgroup %s: %w", path, err)
	}
	return &linuxGroup{file: file, path: path}, nil
}

// PrepareWithFallback retains a hard per-process data-segment limit when a
// host exposes cgroup v2 but does not delegate a writable subtree, as is
// common in developer containers and WSL. RLIMIT_AS prevents Go and Node from
// reserving their normal runtime address space before boot, so the fallback is
// weaker than the aggregate cgroup limit. The caller records the reason and
// keeps the host-floor watchdog armed.
func PrepareWithFallback(limit uint64) (Group, string, error) {
	group, err := Prepare(limit)
	if err == nil {
		return group, "cgroup-v2", nil
	}
	if limit/1024 == 0 {
		return nil, "", err
	}
	return &linuxFallbackGroup{limit: limit}, "rlimit-data-fallback: " + err.Error(), nil
}

type linuxFallbackGroup struct {
	limit      uint64
	configured bool
}

func (g *linuxFallbackGroup) Kill() error { return nil }

func (g *linuxFallbackGroup) Configure(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("harness containment: nil command")
	}
	if g == nil || g.limit/1024 == 0 {
		return errors.New("harness containment: invalid fallback memory limit")
	}
	if g.configured {
		return errors.New("harness containment: command already configured")
	}
	if cmd.Path == "" || len(cmd.Args) == 0 {
		return errors.New("harness containment: command has no executable")
	}
	args := append([]string(nil), cmd.Args...)
	limit := strconv.FormatUint(g.limit/1024, 10)
	script := `limit="$1"; shift; ulimit -d "$limit" || { printf '%s\n' 'harness containment: setrlimit(RLIMIT_DATA) failed' >&2; exit 125; }; exec "$@"`
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"sh", "-c", script, "agent-overflow-memory-limit", limit}, args...)
	g.configured = true
	return nil
}

func (g *linuxFallbackGroup) Adopt(*exec.Cmd) error { return nil }
func (g *linuxFallbackGroup) Close() error          { return nil }

func pruneOrphanGroups(parent string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return fmt.Errorf("scan harness cgroups: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "agent-overflow-") || !entry.IsDir() {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		procs, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if err != nil {
			return fmt.Errorf("inspect orphan cgroup %s: %w", path, err)
		}
		if strings.TrimSpace(string(procs)) != "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove orphan cgroup %s: %w", path, err)
		}
	}
	return nil
}

func (g *linuxGroup) Configure(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("harness containment: nil command")
	}
	if g == nil || g.file == nil {
		return errors.New("harness containment: cgroup is closed")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(g.file.Fd())
	return nil
}

func (g *linuxGroup) Adopt(*exec.Cmd) error { return nil }

func (g *linuxGroup) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return nil
	}
	g.done = true
	var errs []error
	if g.file != nil {
		if err := g.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close cgroup: %w", err))
		}
	}
	// A leader can exit while a provider or browser descendant remains. The
	// group is private to this launch, so cgroup.kill is the only safe cleanup
	// that cannot strand a descendant outside the supervisor's ownership.
	if err := writeCgroupValue(filepath.Join(g.path, "cgroup.kill"), "1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("kill cgroup descendants: %w", err))
	}
	var removeErr error
	for attempt := 0; attempt < 20; attempt++ {
		removeErr = os.Remove(g.path)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if removeErr != nil {
		errs = append(errs, fmt.Errorf("remove cgroup %s: %w", g.path, removeErr))
	}
	return errors.Join(errs...)
}

func writeCgroupValue(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		return err
	}
	return nil
}

func currentCgroupPath() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	var rel string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			rel = parts[2]
			break
		}
	}
	if rel == "" {
		return "", errors.New("harness containment: cgroup v2 is not active")
	}
	// The unified hierarchy is mounted at /sys/fs/cgroup on supported Linux
	// systems. Resolve the mount instead of accepting a caller-selected path.
	mount, err := unifiedMountpoint()
	if err != nil {
		return "", err
	}
	return filepath.Join(mount, filepath.FromSlash(strings.TrimPrefix(rel, "/"))), nil
}

func unifiedMountpoint() (string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "cgroup2 ") {
			continue
		}
		left := strings.Fields(parts[0])
		if len(left) < 5 {
			continue
		}
		return filepath.FromSlash(unescapeMountField(left[4])), nil
	}
	return "", errors.New("harness containment: cgroup v2 mount not found")
}

func unescapeMountField(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, "\\").Replace(value)
}
