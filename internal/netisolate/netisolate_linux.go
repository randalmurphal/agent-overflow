//go:build linux

package netisolate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"agent-overflow/internal/goenvfile"
	"agent-overflow/internal/ownedtemp"

	"github.com/jsimonetti/rtnetlink"
	"golang.org/x/sys/unix"
)

// LANAddress is the namespace's only non-loopback address. Tests that bind
// the machine's LAN bind and dial it. The namespace has no default route, so
// nothing addressed off this subnet or loopback can leave it.
var LANAddress = net.IPv4(10, 203, 0, 2).To4()

const (
	// LANName is the dummy interface that carries LANAddress.
	LANName   = "lan0"
	LANPrefix = 24
)

const (
	// keepTempArg tells the helper to keep the temp directory the namespace
	// it starts in already has, rather than mount its own.
	keepTempArg = "--keep-temp"
	// goWorkArg names the directory on disk the helper mounts for the go
	// command's work directories (newGoWorkDir).
	goWorkArg = "--go-work="
	// diskTempArg names the directory on disk the helper mounts as the temp
	// directory instead of a tmpfs (reserveRAMTemp).
	diskTempArg = "--disk-temp="
	// diskDirArg names the run's disk directory, which the helper mounts at
	// DiskDir.
	diskDirArg = "--disk-dir="
	// ownedArg names a run directory (ownedtemp), one per argument, that
	// the helper keeps owned while its mount namespace has a process.
	ownedArg = "--owned="
)

// TempDir is where the namespace mounts its private temp directory, and
// what TMPDIR names inside it. It is the same on every run, RAM or disk,
// because the Go test cache records the TMPDIR a test reads; a per-run path
// would make every cached result miss. It is short because a Unix socket path, often under
// t.TempDir, holds at most 107 bytes. The directory itself is an empty
// mount point on the host; /tmp stays as it is inside the namespace.
func TempDir() string { return "/tmp/aot-" + strconv.Itoa(os.Getuid()) }

// Command makes command start inside new user, network and mount
// namespaces. The calling executable must dispatch HelperArg (see the
// package doc), because the namespace's first process is that executable
// re-run as the helper. The process keeps the invoking uid and gid.
// CAP_NET_ADMIN and CAP_SYS_ADMIN are ambient only so the helper can build
// the namespace's interfaces and mount its temp directories; the helper
// clears them before it execs the target. Started inside a namespace
// Command built (Inside), the new namespace keeps that namespace's temp
// directories, as a nested run shares its files. Call before containment
// configures command: the rlimit fallback wraps Path and Args, and the
// cgroup variant adds to SysProcAttr.
//
// The run's directories on disk (the Go work directory, and the temp
// directory when the run is not admitted to a RAM one) and its RAM
// reservation (reserveRAMTemp) stay owned until every process of the run
// has exited. The launcher holds their locks until release; their lock
// files go to command through ExtraFiles, kept by every descendant that
// does not close inherited descriptors; and the helper keeps them owned
// while the run's mount namespace, which every descendant inherits, has a
// process (ownedtemp.KeepForMountNamespace). Command first removes those
// of earlier runs that are free, and says on stderr when this run's temp
// directory is on disk and why, and what the sweep could not do.
//
// The command's first process gets SIGTERM when the thread that started it
// exits, so a killed launcher ends its run. Start command from a goroutine
// locked to its OS thread (runtime.LockOSThread) until it has been waited
// for: Go ends a thread only when a goroutine locked to it exits.
//
// The caller calls release once command has exited, whatever ended it. It
// removes the run's directories, or reports that a process of the run
// still holds them and leaves them for a later run's sweep.
func Command(command *exec.Cmd, stderr io.Writer) (release func() error, err error) {
	noop := func() error { return nil }
	if Inside() {
		return noop, isolate(command, helperOptions{keepTemp: true})
	}
	sweepRunDirs(stderr)
	var dirs []*ownedtemp.Shared
	release = func() error {
		var errs []error
		for _, dir := range dirs {
			errs = append(errs, dir.Release())
		}
		return errors.Join(errs...)
	}
	var options helperOptions
	goWork, err := newGoWorkDir()
	if err != nil {
		return noop, err
	}
	if goWork != nil {
		dirs = append(dirs, goWork)
		options.goWork = goWork.Dir
	}
	diskDir, err := ownedtemp.MakeShared(diskParent(), diskDirPrefix)
	if err != nil {
		return noop, errors.Join(fmt.Errorf("create the run's disk directory: %w", err), release())
	}
	dirs = append(dirs, diskDir)
	options.diskDir = diskDir.Dir
	if reservation, reason := reserveRAMTemp(stderr); reservation != nil {
		dirs = append(dirs, reservation)
	} else {
		diskTemp, err := ownedtemp.MakeShared(diskParent(), diskTempPrefix)
		if err != nil {
			return noop, errors.Join(fmt.Errorf("create the temp directory on disk: %w", err), release())
		}
		dirs = append(dirs, diskTemp)
		options.diskTemp = diskTemp.Dir
		warn(stderr, fmt.Sprintf("netisolate: this run's temp directory is on disk (%s): %s", diskTemp.Dir, reason))
	}
	for _, dir := range dirs {
		options.owned = append(options.owned, dir.Dir)
	}
	if err := isolate(command, options); err != nil {
		return noop, errors.Join(err, release())
	}
	for _, dir := range dirs {
		command.ExtraFiles = append(command.ExtraFiles, dir.Lock)
	}
	return release, nil
}

// helperOptions are the helper's arguments before the target's path.
type helperOptions struct {
	keepTemp bool
	goWork   string
	diskTemp string
	diskDir  string
	owned    []string
}

func (o helperOptions) args() []string {
	var args []string
	if o.keepTemp {
		args = append(args, keepTempArg)
	}
	if o.goWork != "" {
		args = append(args, goWorkArg+o.goWork)
	}
	if o.diskTemp != "" {
		args = append(args, diskTempArg+o.diskTemp)
	}
	if o.diskDir != "" {
		args = append(args, diskDirArg+o.diskDir)
	}
	for _, dir := range o.owned {
		args = append(args, ownedArg+dir)
	}
	return args
}

func parseHelperOptions(args []string) (helperOptions, []string) {
	var o helperOptions
	for len(args) > 0 {
		switch {
		case args[0] == keepTempArg:
			o.keepTemp = true
		case strings.HasPrefix(args[0], goWorkArg):
			o.goWork = strings.TrimPrefix(args[0], goWorkArg)
		case strings.HasPrefix(args[0], diskTempArg):
			o.diskTemp = strings.TrimPrefix(args[0], diskTempArg)
		case strings.HasPrefix(args[0], diskDirArg):
			o.diskDir = strings.TrimPrefix(args[0], diskDirArg)
		case strings.HasPrefix(args[0], ownedArg):
			o.owned = append(o.owned, strings.TrimPrefix(args[0], ownedArg))
		default:
			return o, args
		}
		args = args[1:]
	}
	return o, args
}

func isolate(command *exec.Cmd, options helperOptions) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	args := append([]string{self, HelperArg}, options.args()...)
	command.Args = append(append(args, command.Path, "--"), command.Args...)
	command.Path = self
	enterIsolatedNamespaces(command, !options.keepTemp)
	return nil
}

func enterIsolatedNamespaces(command *exec.Cmd, privateTemp bool) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	attr := command.SysProcAttr
	attr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
	attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
	attr.GidMappingsEnableSetgroups = false
	// The setting survives the helper's exec of the target: neither exec
	// gains capabilities or changes ids, which would clear it
	// (TestIsolatedCommandGetsAPrivateRAMTempDir).
	attr.Pdeathsig = syscall.SIGTERM
	attr.AmbientCaps = append(attr.AmbientCaps, unix.CAP_NET_ADMIN)
	if privateTemp {
		attr.Cloneflags |= syscall.CLONE_NEWNS
		attr.AmbientCaps = append(attr.AmbientCaps, unix.CAP_SYS_ADMIN)
	}
}

// Check builds a throwaway namespace, so a host that refuses unprivileged
// user namespaces or the temp directory's mount fails a run up front with
// the reason instead of falling back to the host network or disk.
func Check(stderr io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	// No target command: the helper builds the namespace and exits.
	command := exec.Command(self, HelperArg)
	command.Stderr = stderr
	enterIsolatedNamespaces(command, true)
	if err := command.Run(); err != nil {
		return fmt.Errorf("create an isolated namespace (unprivileged user namespaces must be enabled): %w", err)
	}
	return nil
}

// RunHelper runs as the first process inside the namespaces. It brings up
// loopback and the isolated LAN interface, mounts the private temp
// directory and the Go work directory, drops the ambient capabilities,
// lowers its CPU priority and replaces itself with the target, which keeps
// the run directories' lock files Command passed. Before any of that it
// keeps each run directory owned while its mount namespace has a process.
// args is `[--keep-temp] [--go-work=DIR] [--disk-temp=DIR] [--owned=DIR...]
// path -- argv...`, or empty for Check.
func RunHelper(args []string, stderr io.Writer) int {
	// Niceness belongs to a thread on Linux. The thread that lowers it must
	// be the one that execs, so the target starts with it.
	runtime.LockOSThread()
	options, args := parseHelperOptions(args)
	for _, dir := range options.owned {
		if err := ownedtemp.KeepForMountNamespace(dir); err != nil {
			warn(stderr, "netisolate:", err)
			return 1
		}
	}
	if err := configureIsolatedNetwork(); err != nil {
		warn(stderr, "netisolate: configure isolated network:", explainConfigureError(err))
		return 1
	}
	// Read before the private temp directory hides a file in the one it
	// replaces.
	var userGoEnv []byte
	if options.goWork != "" {
		var err error
		if userGoEnv, err = goenvfile.Read(); err != nil {
			warn(stderr, "netisolate:", err)
			return 1
		}
	}
	if !options.keepTemp {
		if err := mountPrivateTemp(TempDir(), options.diskTemp, tempDirBytes); err != nil {
			warn(stderr, "netisolate: mount the private temp directory:", explainMountError(err))
			return 1
		}
	}
	if options.goWork != "" {
		if err := mountGoWork(options.goWork, userGoEnv); err != nil {
			warn(stderr, "netisolate: mount the Go work directory:", explainMountError(err))
			return 1
		}
	}
	if options.diskDir != "" {
		if err := mountDiskDir(options.diskDir); err != nil {
			warn(stderr, "netisolate: mount the run's disk directory:", explainMountError(err))
			return 1
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		warn(stderr, "netisolate: clear ambient capabilities:", err)
		return 1
	}
	if len(args) == 0 {
		return 0
	}
	if len(args) < 3 || args[1] != "--" {
		warn(stderr, "netisolate: helper needs `[--keep-temp] [--go-work=DIR] [--disk-temp=DIR] [--disk-dir=DIR] [--owned=DIR...] path -- argv...`")
		return 2
	}
	if err := unix.Setpriority(unix.PRIO_PROCESS, 0, Niceness); err != nil {
		warn(stderr, fmt.Sprintf("netisolate: run at normal CPU priority: lower it to nice %d: %v", Niceness, err))
	}
	err := recordNamespace()
	if err == nil && !options.keepTemp {
		err = pointTempDir(TempDir())
	}
	if err == nil && options.goWork != "" {
		err = os.Setenv("GOENV", goEnvPath())
	}
	if err == nil {
		err = syscall.Exec(args[0], args[2:], os.Environ())
	}
	warn(stderr, "netisolate: start isolated command:", err)
	return 1
}

// mountPrivateTemp mounts the run's temp directory on dir, in this mount
// namespace only: diskDir when the run's is on disk, else a tmpfs of at
// most size bytes. Exec stays allowed: go test runs test binaries from its
// temp directory.
func mountPrivateTemp(dir, diskDir string, size int64) error {
	// Mounts made here must not reach the host's namespace. A namespace
	// owned by a new user namespace receives the host's shared mounts as
	// slaves already; this states it rather than relies on it.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("stop mount propagation to the host: %w", err)
	}
	if err := ensureMountPoint(dir); err != nil {
		return err
	}
	if diskDir != "" {
		if err := unix.Mount(diskDir, dir, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mount %s on %s: %w", diskDir, dir, err)
		}
		return nil
	}
	options := fmt.Sprintf("size=%d,mode=0700", size)
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, options); err != nil {
		return fmt.Errorf("mount a tmpfs on %s: %w", dir, err)
	}
	return nil
}

// ensureMountPoint creates dir, or accepts it when it is already a
// directory this user owns. A mount follows a symlink, and another user's
// directory could be swapped under the mount, so anything else is refused.
func ensureMountPoint(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory; remove it", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by this user; remove it", dir)
	}
	return nil
}

// pointTempDir sets TMPDIR to dir for the target and records the temp
// directory it replaced in EnvHostTempDir.
func pointTempDir(dir string) error {
	if err := os.Setenv(EnvHostTempDir, hostTempDir()); err != nil {
		return fmt.Errorf("record the host temp directory: %w", err)
	}
	if err := os.Setenv("TMPDIR", dir); err != nil {
		return fmt.Errorf("set TMPDIR: %w", err)
	}
	return nil
}

// explainMountError names the usual cause of EPERM from the mount: the
// host lets the user namespace exist but refuses it mounts, as Docker's
// default AppArmor profile and seccomp filter do.
func explainMountError(err error) error {
	if !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) {
		return err
	}
	return fmt.Errorf("%w (the host refuses mounts in an unprivileged user namespace; in a container, run it privileged or with a profile that allows mount)", err)
}

// explainConfigureError names the usual cause of EPERM here: the kernel
// created the user namespace but gave it no capabilities. Ubuntu 23.10 and
// later do that for unprivileged processes through AppArmor.
func explainConfigureError(err error) error {
	if !errors.Is(err, unix.EPERM) {
		return err
	}
	return fmt.Errorf("%w (the user namespace holds no capabilities; on Ubuntu 23.10 and later AppArmor restricts unprivileged user namespaces while kernel.apparmor_restrict_unprivileged_userns is 1)", err)
}

func configureIsolatedNetwork() error {
	conn, err := rtnetlink.Dial(nil)
	if err != nil {
		return fmt.Errorf("open netlink: %w", err)
	}
	defer conn.Close()
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}
	if err := setLinkUp(conn, lo.Index, 0); err != nil {
		return fmt.Errorf("bring up loopback: %w", err)
	}
	if err := conn.Link.New(&rtnetlink.LinkMessage{
		Attributes: &rtnetlink.LinkAttributes{Name: LANName, Info: &rtnetlink.LinkInfo{Kind: "dummy"}},
	}); err != nil {
		return fmt.Errorf("create %s: %w", LANName, err)
	}
	lan, err := net.InterfaceByName(LANName)
	if err != nil {
		return fmt.Errorf("find %s: %w", LANName, err)
	}
	if err := conn.Address.New(&rtnetlink.AddressMessage{
		Family:       unix.AF_INET,
		PrefixLength: LANPrefix,
		Index:        uint32(lan.Index),
		Attributes:   &rtnetlink.AddressAttributes{Address: LANAddress, Local: LANAddress},
	}); err != nil {
		return fmt.Errorf("address %s: %w", LANName, err)
	}
	// Multicast like a real LAN interface: nearby discovery only browses and
	// advertises on multicast-capable interfaces.
	if err := setLinkUp(conn, lan.Index, unix.IFF_MULTICAST); err != nil {
		return fmt.Errorf("bring up %s: %w", LANName, err)
	}
	return nil
}

func setLinkUp(conn *rtnetlink.Conn, index int, flags uint32) error {
	if index <= 0 {
		return errors.New("invalid interface index")
	}
	flags |= unix.IFF_UP
	return conn.Link.Set(&rtnetlink.LinkMessage{Index: uint32(index), Flags: flags, Change: flags})
}

func namespaceID() (string, error) { return os.Readlink("/proc/self/ns/net") }

// recordNamespace sets EnvNamespace for the target the helper execs.
func recordNamespace() error {
	id, err := namespaceID()
	if err != nil {
		return fmt.Errorf("read network namespace: %w", err)
	}
	if err := os.Setenv(EnvNamespace, id); err != nil {
		return fmt.Errorf("record network namespace: %w", err)
	}
	return nil
}

// Inside reports whether this process runs in a namespace Command built: its
// interfaces have that shape and EnvNamespace, set by the helper, names this
// process's own network namespace. A host with a lookalike interface set,
// or a process that inherited the variable outside that namespace, is not
// inside. Launchers use it to decide whether a namespace is already there to
// run in; a wrong answer would put listeners on the host.
func Inside() bool {
	want := os.Getenv(EnvNamespace)
	if want == "" {
		return false
	}
	id, err := namespaceID()
	return err == nil && id == want && Contained()
}

// Contained reports whether this process runs inside the namespace Command
// builds, judged by its interfaces: only loopback and LANName at
// LANAddress. Nothing such a process sends can leave the namespace, so an
// isolated boot may use its LAN as a LAN (network.IsolatedReach).
func Contained() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	return containedInterfaces(ifaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() }, LANName, LANAddress)
}
