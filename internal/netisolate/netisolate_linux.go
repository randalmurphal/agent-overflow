//go:build linux

package netisolate

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"

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

// Command makes command start inside new user and network namespaces. The
// calling executable must dispatch HelperArg (see the package doc), because
// the namespace's first process is that executable re-run as the helper.
// The process keeps the invoking uid and gid. CAP_NET_ADMIN is ambient only
// so the helper can build the namespace's interfaces; the helper clears it
// before it execs the target. Call before containment configures command:
// the rlimit fallback wraps Path and Args, and the cgroup variant adds to
// SysProcAttr.
func Command(command *exec.Cmd) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	command.Args = append([]string{self, HelperArg, command.Path, "--"}, command.Args...)
	command.Path = self
	enterIsolatedNamespaces(command)
	return nil
}

func enterIsolatedNamespaces(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	attr := command.SysProcAttr
	attr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
	attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
	attr.GidMappingsEnableSetgroups = false
	attr.AmbientCaps = append(attr.AmbientCaps, unix.CAP_NET_ADMIN)
}

// Check builds a throwaway namespace, so a host that refuses unprivileged
// user namespaces fails a run up front with the reason instead of falling
// back to the host network.
func Check(stderr io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	// No target command: the helper builds the namespace and exits.
	command := exec.Command(self, HelperArg)
	command.Stderr = stderr
	enterIsolatedNamespaces(command)
	if err := command.Run(); err != nil {
		return fmt.Errorf("create an isolated network namespace (unprivileged user namespaces must be enabled): %w", err)
	}
	return nil
}

// RunHelper runs as the first process inside the namespaces. It brings up
// loopback and the isolated LAN interface, drops the ambient capability, and
// replaces itself with the target. args is `path -- argv...`, or empty for
// Check.
func RunHelper(args []string, stderr io.Writer) int {
	if err := configureIsolatedNetwork(); err != nil {
		fmt.Fprintln(stderr, "netisolate: configure isolated network:", err)
		return 1
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		fmt.Fprintln(stderr, "netisolate: clear ambient capabilities:", err)
		return 1
	}
	if len(args) == 0 {
		return 0
	}
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(stderr, "netisolate: helper needs `path -- argv...`")
		return 2
	}
	err := syscall.Exec(args[0], args[2:], os.Environ())
	fmt.Fprintln(stderr, "netisolate: start isolated command:", err)
	return 1
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
