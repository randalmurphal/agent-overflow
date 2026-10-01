//go:build linux

package main

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

// isolatedLANAddress is the suite's only non-loopback address. LAN-bind specs
// bind and dial it as the machine's LAN. The namespace has no default route,
// so nothing addressed off this subnet or loopback can leave it.
var isolatedLANAddress = net.IPv4(10, 203, 0, 2).To4()

const (
	isolatedLANName   = "lan0"
	isolatedLANPrefix = 24
)

// isolateNetwork makes command start inside new user and network namespaces.
// The process keeps the invoking uid and gid. CAP_NET_ADMIN is ambient only so
// the helper can build the namespace's interfaces; the helper clears it before
// it execs the suite. Call before containment configures command: the rlimit
// fallback wraps Path and Args, and the cgroup variant adds to SysProcAttr.
func isolateNetwork(command *exec.Cmd) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve launcher executable: %w", err)
	}
	command.Args = append([]string{self, netnsHelperArg, command.Path, "--"}, command.Args...)
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

// checkNetworkIsolation builds a throwaway namespace before the suite starts,
// so a host that refuses unprivileged user namespaces fails the run with the
// reason instead of falling back to the host network.
func checkNetworkIsolation(stderr io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve launcher executable: %w", err)
	}
	// No target command: the helper builds the namespace and exits.
	command := exec.Command(self, netnsHelperArg)
	command.Stderr = stderr
	enterIsolatedNamespaces(command)
	if err := command.Run(); err != nil {
		return fmt.Errorf("create an isolated network namespace (unprivileged user namespaces must be enabled; --host-network is only for suites that need host services): %w", err)
	}
	return nil
}

// runNetnsHelper runs as the first process inside the namespaces. It brings
// up loopback and the isolated LAN interface, drops the ambient capability,
// and replaces itself with the target. args is `path -- argv...`, or empty
// for checkNetworkIsolation.
func runNetnsHelper(args []string, stderr io.Writer) int {
	if err := configureIsolatedNetwork(); err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: configure isolated network:", err)
		return 1
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		fmt.Fprintln(stderr, "ao-harness-e2e: clear ambient capabilities:", err)
		return 1
	}
	if len(args) == 0 {
		return 0
	}
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(stderr, "ao-harness-e2e: network helper needs `path -- argv...`")
		return 2
	}
	err := syscall.Exec(args[0], args[2:], os.Environ())
	fmt.Fprintln(stderr, "ao-harness-e2e: start isolated suite:", err)
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
		Attributes: &rtnetlink.LinkAttributes{Name: isolatedLANName, Info: &rtnetlink.LinkInfo{Kind: "dummy"}},
	}); err != nil {
		return fmt.Errorf("create %s: %w", isolatedLANName, err)
	}
	lan, err := net.InterfaceByName(isolatedLANName)
	if err != nil {
		return fmt.Errorf("find %s: %w", isolatedLANName, err)
	}
	if err := conn.Address.New(&rtnetlink.AddressMessage{
		Family:       unix.AF_INET,
		PrefixLength: isolatedLANPrefix,
		Index:        uint32(lan.Index),
		Attributes:   &rtnetlink.AddressAttributes{Address: isolatedLANAddress, Local: isolatedLANAddress},
	}); err != nil {
		return fmt.Errorf("address %s: %w", isolatedLANName, err)
	}
	// Multicast like a real LAN interface: nearby discovery only browses and
	// advertises on multicast-capable interfaces.
	if err := setLinkUp(conn, lan.Index, unix.IFF_MULTICAST); err != nil {
		return fmt.Errorf("bring up %s: %w", isolatedLANName, err)
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
