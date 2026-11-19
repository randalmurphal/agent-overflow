//go:build linux

package netisolate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const isolationProbeEnv = "AO_NETISOLATE_PROBE"

// TestMain lets the test binary stand in for a caller: Command re-executes
// os.Executable() as the namespace helper, and the helper then execs the
// binary again as the probe.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		os.Exit(RunHelper(os.Args[2:], os.Stderr))
	}
	// Every launch in the test binary and the processes it starts takes its
	// RAM reservation in a directory of the test's own, never the host's.
	reservationsDir = func() string { return os.Getenv(reservationsEnv) }
	if os.Getenv(reservationsEnv) == "" {
		os.Exit(withTestReservations(m))
	}
	switch os.Getenv(isolationProbeEnv) {
	case "1":
		os.Exit(runIsolationProbe(os.Stdout))
	case "temp":
		os.Exit(runTempProbe(os.Stdout))
	case "temp-hold":
		code := runTempProbe(os.Stdout)
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(code)
	case "nested":
		os.Exit(runNestedTempProbe(os.Stdout))
	case "launcher":
		os.Exit(runLauncher(memoryLimits{available: 0, total: 64 << 30}))
	case "ram-launcher":
		os.Exit(runLauncher(budgetFor(1)))
	case "linger":
		os.Exit(runLinger(os.Stdout))
	case "spawn":
		os.Exit(runSpawn())
	case "hold":
		os.Exit(runHold())
	}
	os.Exit(m.Run())
}

// reservationsEnv names the directory that stands in for reservationsDir
// in the test binary and every process it starts.
const reservationsEnv = "AO_NETISOLATE_TEST_RESERVATIONS"

// withTestReservations runs the tests with reservationsEnv naming a fresh
// directory on the host's /tmp, which every namespace shares, and removes
// it afterwards.
func withTestReservations(m *testing.M) int {
	dir, err := os.MkdirTemp("/tmp", "netisolate-reservations-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Setenv(reservationsEnv, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return code
}

type probeInterface struct {
	Name  string
	Flags string
	Addrs []string
}

type isolationProbe struct {
	Interfaces     []probeInterface
	CapEff         string
	Routes         []string
	OffLANDial     string
	OffLANDialMs   int64
	LoopbackV4Dial string
	LoopbackV6Dial string
	LANDial        string
	Contained      bool
	Inside         bool
}

// runIsolationProbe reports what an isolated command would see. It dials off the LAN
// only after confirming that lo and lan0 are the only interfaces, so a broken
// namespace fails the test without sending anything to a real network.
func runIsolationProbe(out io.Writer) int {
	var probe isolationProbe
	ifaces, err := net.Interfaces()
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe interfaces:", err)
		return 1
	}
	isolated := true
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		entry := probeInterface{Name: iface.Name, Flags: iface.Flags.String()}
		for _, addr := range addrs {
			entry.Addrs = append(entry.Addrs, addr.String())
		}
		probe.Interfaces = append(probe.Interfaces, entry)
		if iface.Name != "lo" && iface.Name != LANName {
			isolated = false
		}
	}
	probe.CapEff = procStatusField("CapEff")
	probe.Routes = ipv4Routes()
	if isolated {
		start := time.Now()
		// TEST-NET-1: never a real destination even if routing were wrong.
		conn, err := net.DialTimeout("tcp4", "192.0.2.1:80", 3*time.Second)
		probe.OffLANDialMs = time.Since(start).Milliseconds()
		probe.OffLANDial = dialOutcome(conn, err)
	}
	probe.LoopbackV4Dial = selfDial("tcp4", "127.0.0.1:0")
	probe.LoopbackV6Dial = selfDial("tcp6", "[::1]:0")
	probe.LANDial = selfDial("tcp4", net.JoinHostPort(LANAddress.String(), "0"))
	probe.Contained = Contained()
	probe.Inside = Inside()
	if err := json.NewEncoder(out).Encode(probe); err != nil {
		return 1
	}
	return 0
}

func dialOutcome(conn net.Conn, err error) string {
	switch {
	case err == nil:
		conn.Close()
		return "connected"
	case errors.Is(err, syscall.ENETUNREACH):
		return "ENETUNREACH"
	default:
		return err.Error()
	}
}

func selfDial(network, address string) string {
	ln, err := net.Listen(network, address)
	if err != nil {
		return "listen: " + err.Error()
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.Close()
		}
	}()
	conn, err := net.DialTimeout(network, ln.Addr().String(), 3*time.Second)
	return dialOutcome(conn, err)
}

func procStatusField(name string) string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "unreadable: " + err.Error()
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, name+":"); ok {
			return strings.TrimSpace(value)
		}
	}
	return "missing"
}

// ipv4Routes lists main-table destinations as hex/mask pairs from
// /proc/net/route; a default route shows as 00000000/00000000.
func ipv4Routes() []string {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return []string{"unreadable: " + err.Error()}
	}
	defer file.Close()
	var routes []string
	scanner := bufio.NewScanner(file)
	scanner.Scan() // header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 8 {
			routes = append(routes, fields[0]+" "+fields[1]+"/"+fields[7])
		}
	}
	return routes
}

func runProbe(t *testing.T) isolationProbe {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(self, "-test.run=^$")
	command.Env = append(os.Environ(), isolationProbeEnv+"=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	release, err := Command(command, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	if err := command.Run(); err != nil {
		t.Fatalf("isolated probe: %v\n%s", err, stderr.String())
	}
	var probe isolationProbe
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		t.Fatalf("decode probe %q: %v", stdout.String(), err)
	}
	return probe
}

func TestIsolatedSuiteSeesOnlyLoopbackAndPrivateLAN(t *testing.T) {
	probe := runProbe(t)
	if len(probe.Interfaces) != 2 {
		t.Fatalf("interfaces = %+v, want exactly lo and %s", probe.Interfaces, LANName)
	}
	for _, iface := range probe.Interfaces {
		switch iface.Name {
		case "lo":
			if !strings.Contains(iface.Flags, "up") || !strings.Contains(iface.Flags, "loopback") {
				t.Fatalf("lo flags = %s, want up loopback", iface.Flags)
			}
		case LANName:
			// LAN discovery (network.DiscoverLocalLANIP) requires up and
			// running; nearby discovery requires multicast.
			for _, flag := range []string{"up", "running", "multicast"} {
				if !strings.Contains(iface.Flags, flag) {
					t.Fatalf("%s flags = %s, want up running multicast", iface.Name, iface.Flags)
				}
			}
			want := fmt.Sprintf("%s/%d", LANAddress, LANPrefix)
			found := false
			for _, addr := range iface.Addrs {
				found = found || addr == want
			}
			if !found {
				t.Fatalf("%s addrs = %v, want %s", iface.Name, iface.Addrs, want)
			}
		default:
			t.Fatalf("unexpected interface %+v", iface)
		}
	}
	if want := []string{LANName + " 0000CB0A/00FFFFFF"}; strings.Join(probe.Routes, ",") != strings.Join(want, ",") {
		t.Fatalf("IPv4 routes = %v, want only the LAN subnet %v", probe.Routes, want)
	}
	if probe.OffLANDial != "ENETUNREACH" || probe.OffLANDialMs > 1000 {
		t.Fatalf("off-LAN dial = %q after %dms, want an immediate ENETUNREACH", probe.OffLANDial, probe.OffLANDialMs)
	}
	if !probe.Contained {
		t.Fatal("Contained() is false inside the namespace")
	}
	if !probe.Inside {
		t.Fatal("Inside() is false inside the namespace")
	}
	for name, outcome := range map[string]string{"127.0.0.1": probe.LoopbackV4Dial, "::1": probe.LoopbackV6Dial, "lan0": probe.LANDial} {
		if outcome != "connected" {
			t.Fatalf("dial %s = %q, want connected", name, outcome)
		}
	}
}

// TestInsideRequiresTheHelpersOwnNamespace: the variable alone, or a value
// naming another namespace, does not make a process inside. The nested
// namespace the probe runs in proves the true case.
func TestInsideRequiresTheHelpersOwnNamespace(t *testing.T) {
	id, err := namespaceID()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvNamespace, "")
	if Inside() {
		t.Fatal("Inside() without the variable")
	}
	t.Setenv(EnvNamespace, "net:[1]")
	if Inside() {
		t.Fatal("Inside() with another namespace's identity")
	}
	t.Setenv(EnvNamespace, id)
	if Inside() != Contained() {
		t.Fatalf("Inside() = %v with this namespace's identity, want Contained() = %v", Inside(), Contained())
	}
}

func TestIsolatedSuiteHoldsNoCapabilities(t *testing.T) {
	if capEff := runProbe(t).CapEff; capEff != "0000000000000000" {
		t.Fatalf("CapEff = %s, want none after the helper execs the suite", capEff)
	}
}

func TestCheckBuildsANamespace(t *testing.T) {
	var stderr bytes.Buffer
	if err := Check(&stderr); err != nil {
		t.Fatalf("Check: %v\n%s", err, stderr.String())
	}
}

// TestExplainConfigureErrorNamesTheAppArmorRestriction: a namespace without
// capabilities fails its first netlink request with EPERM, which alone does
// not say why.
func TestExplainConfigureErrorNamesTheAppArmorRestriction(t *testing.T) {
	eperm := fmt.Errorf("bring up loopback: %w", &net.OpError{Op: "receive", Err: syscall.EPERM})
	got := explainConfigureError(eperm)
	if !errors.Is(got, syscall.EPERM) || !strings.Contains(got.Error(), "apparmor_restrict_unprivileged_userns") {
		t.Fatalf("EPERM explained as %v", got)
	}
	other := errors.New("find loopback: no such interface")
	if got := explainConfigureError(other); got != other {
		t.Fatalf("unrelated error rewritten to %v", got)
	}
}

// hostFileEnv names a file outside the namespace that the temp probe reads.
const hostFileEnv = "AO_NETISOLATE_PROBE_HOST_FILE"

type tempProbe struct {
	TempDir      string
	HostTempDir  string
	TempFS       bool
	TempBytes    int64
	MountNS      string
	Written      string
	WriteError   string
	HostFile     string
	Niceness     string
	CapEff       string
	GoEnv        string
	GoEnvFile    string
	GoTmpDirEnv  string
	GoWorkFile   string
	NestedTemp   string
	NestedSeen   bool
	NestedStderr string
	PdeathSig    int
}

// runTempProbe reports the temp directory the isolated command sees and
// leaves a file in it that must not outlive the namespace.
func runTempProbe(out io.Writer) int {
	probe := tempProbe{TempDir: os.Getenv("TMPDIR"), HostTempDir: os.Getenv(EnvHostTempDir)}
	var stat unix.Statfs_t
	if err := unix.Statfs(probe.TempDir, &stat); err == nil {
		probe.TempFS = stat.Type == unix.TMPFS_MAGIC
		probe.TempBytes = int64(stat.Blocks) * stat.Bsize
	}
	probe.MountNS, _ = os.Readlink("/proc/self/ns/mnt")
	if file, err := os.CreateTemp("", "netisolate-probe-"); err != nil {
		probe.WriteError = err.Error()
	} else {
		probe.Written = file.Name()
		file.Close()
	}
	if data, err := os.ReadFile(os.Getenv(hostFileEnv)); err != nil {
		probe.HostFile = "unreadable: " + err.Error()
	} else {
		probe.HostFile = string(data)
	}
	probe.Niceness = procStatNice()
	probe.CapEff = procStatusField("CapEff")
	probe.GoEnv = os.Getenv("GOENV")
	probe.GoTmpDirEnv = os.Getenv("GOTMPDIR")
	if data, err := os.ReadFile(probe.GoEnv); err == nil {
		probe.GoEnvFile = string(data)
	}
	if file, err := os.CreateTemp(goWorkPath(), "probe-work-"); err == nil {
		probe.GoWorkFile = file.Name()
		file.Close()
	}
	probe.PdeathSig = parentDeathSignal()
	return encodeProbe(out, probe)
}

// parentDeathSignal is this process's PR_GET_PDEATHSIG, or -1 when it
// cannot be read.
func parentDeathSignal() int {
	var sig int32
	if err := unix.Prctl(unix.PR_GET_PDEATHSIG, uintptr(unsafe.Pointer(&sig)), 0, 0, 0); err != nil {
		return -1
	}
	return int(sig)
}

// runNestedTempProbe runs a temp probe through Command from inside a
// namespace Command built, after leaving a file in its own temp directory.
func runNestedTempProbe(out io.Writer) int {
	probe := tempProbe{TempDir: os.Getenv("TMPDIR")}
	file, err := os.CreateTemp("", "netisolate-outer-")
	if err != nil {
		probe.WriteError = err.Error()
		return encodeProbe(out, probe)
	}
	probe.Written = file.Name()
	file.Close()
	self, err := os.Executable()
	if err != nil {
		probe.NestedStderr = err.Error()
		return encodeProbe(out, probe)
	}
	nested, stderr, err := startTempProbe(self, "temp", func(command *exec.Cmd) error {
		release, err := Command(command, os.Stderr)
		if err != nil {
			return err
		}
		return release()
	})
	if err != nil {
		probe.NestedStderr = err.Error() + "\n" + stderr
		return encodeProbe(out, probe)
	}
	probe.NestedTemp = nested.TempDir
	_, statErr := os.Stat(filepath.Join(nested.TempDir, filepath.Base(probe.Written)))
	// The nested probe's own file stays visible here only if it wrote into
	// this namespace's temp directory.
	_, nestedErr := os.Stat(nested.Written)
	probe.NestedSeen = statErr == nil && nestedErr == nil
	return encodeProbe(out, probe)
}

func encodeProbe(out io.Writer, probe any) int {
	if err := json.NewEncoder(out).Encode(probe); err != nil {
		return 1
	}
	return 0
}

// procStatNice is the nice field of /proc/self/stat, the 19th, counted
// after the parenthesized command name.
func procStatNice() string {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return "unreadable: " + err.Error()
	}
	text := string(data)
	// fields[0] is the state, the 3rd field of the line.
	fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
	if len(fields) < 17 {
		return "short: " + text
	}
	return fields[16]
}

// startTempProbe runs binary, a copy of this test binary, as a temp probe
// in role, isolated by isolate.
func startTempProbe(binary, role string, isolate func(*exec.Cmd) error) (tempProbe, string, error) {
	command := exec.Command(binary, "-test.run=^$")
	command.Env = append(os.Environ(), isolationProbeEnv+"="+role)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := isolate(command); err != nil {
		return tempProbe{}, "", err
	}
	if err := command.Run(); err != nil {
		return tempProbe{}, stderr.String(), fmt.Errorf("isolated probe: %w", err)
	}
	var probe tempProbe
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		return tempProbe{}, stderr.String(), fmt.Errorf("decode probe %q: %w", stdout.String(), err)
	}
	return probe, stderr.String(), nil
}

// privateNamespace isolates a command in a namespace with a temp directory
// of its own even when the test already runs inside one, as it does under
// make go-test.
func privateNamespace(command *exec.Cmd) error { return isolate(command, helperOptions{}) }

// privateNamespaceWithGoWork is privateNamespace with a Go work directory
// on disk, as Command gives a namespace of its own. The developer's Go
// environment file is userEnv.
func privateNamespaceWithGoWork(t *testing.T, userEnv string) (isolateCommand func(*exec.Cmd) error, goWork string) {
	t.Helper()
	userFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(userFile, []byte(userEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", userFile)
	t.Setenv("GOTMPDIR", "")
	dir, err := newGoWorkDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatal("no Go work directory")
	}
	t.Cleanup(func() {
		if err := dir.Release(); err != nil {
			t.Error(err)
		}
	})
	return func(command *exec.Cmd) error { return isolate(command, helperOptions{goWork: dir.Dir}) }, dir.Dir
}

// probeBinary copies this test binary to a directory under /tmp. Under
// make go-test the binary sits in go test's work directory inside the
// outer namespace's temp directory, which a private namespace's own mount
// hides from the probe it starts.
func probeBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "netisolate-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove probe binary: %v", err)
		}
	})
	binary := filepath.Join(dir, "probe")
	if err := os.WriteFile(binary, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}

// mountNamespacesInUse lists the mount namespaces of the processes this
// user can inspect.
func mountNamespacesInUse() map[string]bool {
	seen := map[string]bool{}
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		if id, err := os.Readlink(filepath.Join("/proc", entry.Name(), "ns", "mnt")); err == nil {
			seen[id] = true
		}
	}
	return seen
}

// TestIsolatedCommandGetsAPrivateRAMTempDir: TMPDIR names a size-capped
// tmpfs at the fixed TempDir, whose files nobody outside the namespace sees
// and which is gone with the namespace; /tmp stays the host's, so a path a
// caller passes in is readable; and the command runs at nice 19.
func TestIsolatedCommandGetsAPrivateRAMTempDir(t *testing.T) {
	hostFile, err := os.CreateTemp("/tmp", "netisolate-host-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(hostFile.Name()) })
	if _, err := hostFile.WriteString("from the host"); err != nil {
		t.Fatal(err)
	}
	if err := hostFile.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hostFileEnv, hostFile.Name())

	ownMountNS, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	isolateCommand, goWork := privateNamespaceWithGoWork(t, "GOPROXY=off")
	probe, stderr, err := startTempProbe(probeBinary(t), "temp", isolateCommand)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if probe.TempDir != TempDir() {
		t.Fatalf("TMPDIR = %q, want the fixed %q", probe.TempDir, TempDir())
	}
	if !probe.TempFS || probe.TempBytes != tempDirBytes {
		t.Fatalf("TMPDIR is tmpfs=%v of %d bytes, want a tmpfs of %d", probe.TempFS, probe.TempBytes, int64(tempDirBytes))
	}
	if probe.HostTempDir == "" || probe.HostTempDir == TempDir() {
		t.Fatalf("%s = %q, want the temp directory outside the namespace", EnvHostTempDir, probe.HostTempDir)
	}
	if !strings.HasPrefix(probe.Written, TempDir()+"/") {
		t.Fatalf("probe wrote %q (%s), want a file under %s", probe.Written, probe.WriteError, TempDir())
	}
	if _, err := os.Stat(probe.Written); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file the namespace wrote is visible outside it: stat %s: %v", probe.Written, err)
	}
	if probe.HostFile != "from the host" {
		t.Fatalf("host file read inside = %q, want its content", probe.HostFile)
	}
	// Under make go-test this process runs at nice 19 already and the probe
	// inherits it; run the test binary at normal priority to see the
	// helper lower it.
	if probe.Niceness != strconv.Itoa(Niceness) {
		t.Fatalf("niceness = %s, want %d", probe.Niceness, Niceness)
	}
	// The helper mounted with CAP_SYS_ADMIN; the command holds nothing.
	if probe.CapEff != "0000000000000000" {
		t.Fatalf("CapEff = %s, want none after the helper mounts the temp directory", probe.CapEff)
	}
	// Set on the helper, and kept through its exec of the command.
	if probe.PdeathSig != int(syscall.SIGTERM) {
		t.Fatalf("parent-death signal = %d, want SIGTERM (%d)", probe.PdeathSig, syscall.SIGTERM)
	}
	if probe.MountNS == "" || probe.MountNS == ownMountNS {
		t.Fatalf("probe mount namespace %q, want one of its own (this process: %s)", probe.MountNS, ownMountNS)
	}
	if mountNamespacesInUse()[probe.MountNS] {
		t.Fatalf("mount namespace %s, and with it the temp mount, outlived the command", probe.MountNS)
	}
	// The go command's work directory is the run's directory on disk,
	// named only in the Go environment file at a fixed path.
	if probe.GoEnv != goEnvPath() || probe.GoTmpDirEnv != "" {
		t.Fatalf("GOENV = %q and GOTMPDIR = %q, want GOENV %q and no GOTMPDIR", probe.GoEnv, probe.GoTmpDirEnv, goEnvPath())
	}
	if want := "GOPROXY=off\nGOTMPDIR=" + goWorkPath() + "\n"; probe.GoEnvFile != want {
		t.Fatalf("Go environment file = %q, want %q", probe.GoEnvFile, want)
	}
	if probe.GoWorkFile == "" {
		t.Fatal("probe could not write to the Go work directory")
	}
	if _, err := os.Stat(filepath.Join(goWork, filepath.Base(probe.GoWorkFile))); err != nil {
		t.Fatalf("a file in the Go work directory is not in the run's directory on disk: %v", err)
	}
}

// TestNestedCommandKeepsTheOuterTempDir: a command isolated from inside a
// namespace shares that namespace's temp directory, as it shares its files.
func TestNestedCommandKeepsTheOuterTempDir(t *testing.T) {
	probe, stderr, err := startTempProbe(probeBinary(t), "nested", privateNamespace)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if probe.WriteError != "" || probe.NestedStderr != "" {
		t.Fatalf("nested probe failed: %s%s", probe.WriteError, probe.NestedStderr)
	}
	if probe.NestedTemp != TempDir() || !probe.NestedSeen {
		t.Fatalf("nested TMPDIR = %q, shares files with the outer namespace: %v; want %q and shared files", probe.NestedTemp, probe.NestedSeen, TempDir())
	}
}

// TestMountPointMustBeARealDirectory: a mount follows a symlink, so the
// mount point must be a directory of this user's.
func TestMountPointMustBeARealDirectory(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureMountPoint(link); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("symlink accepted as a mount point: %v", err)
	}
	fresh := filepath.Join(dir, "fresh")
	if err := ensureMountPoint(fresh); err != nil {
		t.Fatalf("new mount point: %v", err)
	}
	if err := ensureMountPoint(fresh); err != nil {
		t.Fatalf("existing mount point: %v", err)
	}
}

func TestExplainMountErrorNamesTheRefusal(t *testing.T) {
	got := explainMountError(fmt.Errorf("mount a tmpfs on /tmp/aot-1: %w", syscall.EPERM))
	if !errors.Is(got, syscall.EPERM) || !strings.Contains(got.Error(), "refuses mounts") {
		t.Fatalf("EPERM explained as %v", got)
	}
	other := errors.New("create /tmp/aot-1: read-only file system")
	if got := explainMountError(other); got != other {
		t.Fatalf("unrelated error rewritten to %v", got)
	}
}
