//go:build linux

package main

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
	"strings"
	"syscall"
	"testing"
	"time"
)

const isolationProbeEnv = "AO_E2E_NETNS_PROBE"

// TestMain lets the test binary stand in for the launcher: isolateNetwork
// re-executes os.Executable() as the namespace helper, and the helper then
// execs the binary again as the probe.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == netnsHelperArg {
		os.Exit(runNetnsHelper(os.Args[2:], os.Stderr))
	}
	if os.Getenv(isolationProbeEnv) == "1" {
		os.Exit(runIsolationProbe(os.Stdout))
	}
	os.Exit(m.Run())
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
}

// runIsolationProbe reports what the suite would see. It dials off the LAN
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
		if iface.Name != "lo" && iface.Name != isolatedLANName {
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
	probe.LANDial = selfDial("tcp4", net.JoinHostPort(isolatedLANAddress.String(), "0"))
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
	if err := isolateNetwork(command); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("interfaces = %+v, want exactly lo and %s", probe.Interfaces, isolatedLANName)
	}
	for _, iface := range probe.Interfaces {
		switch iface.Name {
		case "lo":
			if !strings.Contains(iface.Flags, "up") || !strings.Contains(iface.Flags, "loopback") {
				t.Fatalf("lo flags = %s, want up loopback", iface.Flags)
			}
		case isolatedLANName:
			// LAN discovery (network.DiscoverLocalLANIP) requires up and
			// running; nearby discovery requires multicast.
			for _, flag := range []string{"up", "running", "multicast"} {
				if !strings.Contains(iface.Flags, flag) {
					t.Fatalf("%s flags = %s, want up running multicast", iface.Name, iface.Flags)
				}
			}
			want := fmt.Sprintf("%s/%d", isolatedLANAddress, isolatedLANPrefix)
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
	if want := []string{isolatedLANName + " 0000CB0A/00FFFFFF"}; strings.Join(probe.Routes, ",") != strings.Join(want, ",") {
		t.Fatalf("IPv4 routes = %v, want only the LAN subnet %v", probe.Routes, want)
	}
	if probe.OffLANDial != "ENETUNREACH" || probe.OffLANDialMs > 1000 {
		t.Fatalf("off-LAN dial = %q after %dms, want an immediate ENETUNREACH", probe.OffLANDial, probe.OffLANDialMs)
	}
	for name, outcome := range map[string]string{"127.0.0.1": probe.LoopbackV4Dial, "::1": probe.LoopbackV6Dial, "lan0": probe.LANDial} {
		if outcome != "connected" {
			t.Fatalf("dial %s = %q, want connected", name, outcome)
		}
	}
}

func TestIsolatedSuiteHoldsNoCapabilities(t *testing.T) {
	if capEff := runProbe(t).CapEff; capEff != "0000000000000000" {
		t.Fatalf("CapEff = %s, want none after the helper execs the suite", capEff)
	}
}

func TestCheckNetworkIsolationBuildsANamespace(t *testing.T) {
	var stderr bytes.Buffer
	if err := checkNetworkIsolation(&stderr); err != nil {
		t.Fatalf("checkNetworkIsolation: %v\n%s", err, stderr.String())
	}
}
