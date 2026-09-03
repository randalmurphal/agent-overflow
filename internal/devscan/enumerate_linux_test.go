//go:build linux

package devscan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The enumerator is exercised over a FIXTURE proc tree under a temp dir,
// never the machine's own /proc: a test that read the real one would
// assert whatever happened to be listening on the developer's box.

// procFixture builds a proc tree: a socket table, one directory per
// process with its stat line, and a dangling `socket:[inode]` symlink per
// held fd — which is exactly the shape the kernel presents.
type procFixture struct {
	root string
	tcp  []string
	tcp6 []string
}

func newProcFixture(t *testing.T) *procFixture {
	t.Helper()
	return &procFixture{root: t.TempDir()}
}

// listenRow appends one LISTEN row. hexAddr is the kernel's spelling of
// the bound address (see parseHexAddr).
func (f *procFixture) listenRow(v6 bool, hexAddr string, port, inode int) {
	row := fmt.Sprintf("   0: %s:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 %d 1 0000 100 0 0 10 0",
		hexAddr, port, inode)
	if v6 {
		f.tcp6 = append(f.tcp6, row)
		return
	}
	f.tcp = append(f.tcp, row)
}

// establishedRow appends a row in a state other than LISTEN, which the
// parser must skip.
func (f *procFixture) establishedRow(hexAddr string, port, inode int) {
	f.tcp = append(f.tcp, fmt.Sprintf("   1: %s:%04X 0100007F:1F91 01 00000000:00000000 00:00000000 00000000  1000        0 %d 1",
		hexAddr, port, inode))
}

func (f *procFixture) process(t *testing.T, pid, ppid, pgid int, comm string, inodes ...int) {
	t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(pid))
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	stat := fmt.Sprintf("%d (%s) S %d %d 0 0 -1 4194304 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, comm, ppid, pgid)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	for i, inode := range inodes {
		link := filepath.Join(dir, "fd", fmt.Sprint(3+i))
		if err := os.Symlink(fmt.Sprintf("socket:[%d]", inode), link); err != nil {
			t.Fatalf("symlink fd: %v", err)
		}
	}
}

// unreadableProcess writes a stat line but no readable fd directory,
// which is what another user's process looks like from here.
func (f *procFixture) unreadableProcess(t *testing.T, pid int) {
	t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	stat := fmt.Sprintf("%d (other) S 1 %d 0 0 -1 0 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, pid)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
}

func (f *procFixture) write(t *testing.T) string {
	t.Helper()
	netDir := filepath.Join(f.root, "net")
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatalf("mkdir net: %v", err)
	}
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode"
	body := header + "\n"
	for _, row := range f.tcp {
		body += row + "\n"
	}
	if err := os.WriteFile(filepath.Join(netDir, "tcp"), []byte(body), 0o644); err != nil {
		t.Fatalf("write net/tcp: %v", err)
	}
	body6 := header + "\n"
	for _, row := range f.tcp6 {
		body6 += row + "\n"
	}
	if err := os.WriteFile(filepath.Join(netDir, "tcp6"), []byte(body6), 0o644); err != nil {
		t.Fatalf("write net/tcp6: %v", err)
	}
	return f.root
}

// The kernel's spellings, per parseHexAddr: 32-bit words, each printed
// native-endian, so every four-byte group reads backwards.
const (
	hexLoopbackV4   = "0100007F"
	hexWildcardV4   = "00000000"
	hexRoutableV4   = "0A01A8C0" // 192.168.1.10
	hexLoopbackV6   = "00000000000000000000000001000000"
	hexWildcardV6   = "00000000000000000000000000000000"
	hexRoutableV6   = "0000000000000000000000000A01A8C0"
	portDev         = 5173
	portAPI         = 3000
	portOther       = 8080
	portNotOurs     = 9999
	portEstablished = 45678
)

func TestEnumerateKeepsLoopbackAndWildcardListenersOnly(t *testing.T) {
	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, portDev, 100)
	f.listenRow(true, hexWildcardV6, portAPI, 101)
	f.listenRow(true, hexLoopbackV6, portOther, 102)
	f.listenRow(false, hexRoutableV4, portNotOurs, 103)
	f.listenRow(true, hexRoutableV6, portNotOurs+1, 104)
	f.establishedRow(hexLoopbackV4, portEstablished, 105)
	f.process(t, 4242, 1, 4242, "node", 100, 101, 102, 103, 104, 105)
	root := f.write(t)

	listeners, _, err := enumerateListeners(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	var ports []int
	for _, l := range listeners {
		ports = append(ports, l.Port)
		if l.PID != 4242 || l.Comm != "node" {
			t.Errorf("port %d: pid/comm = %d/%q, want 4242/node", l.Port, l.PID, l.Comm)
		}
	}
	sort.Ints(ports)
	want := []int{portAPI, portDev, portOther}
	sort.Ints(want)
	if len(ports) != len(want) {
		t.Fatalf("ports = %v, want %v — a routable bind or a non-LISTEN row leaked through", ports, want)
	}
	for i := range ports {
		if ports[i] != want[i] {
			t.Fatalf("ports = %v, want %v", ports, want)
		}
	}
}

// A process this app cannot read is skipped, never an error: a scan that
// failed whenever the machine had another user's process would never
// succeed on a shared host.
func TestEnumerateSkipsUnreadableProcesses(t *testing.T) {
	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, portDev, 200)
	f.listenRow(false, hexLoopbackV4, portAPI, 201)
	f.process(t, 111, 1, 111, "vite", 200)
	f.unreadableProcess(t, 222)
	root := f.write(t)

	listeners, _, err := enumerateListeners(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(listeners) != 2 {
		t.Fatalf("listeners = %+v, want both ports", listeners)
	}
	for _, l := range listeners {
		switch l.Port {
		case portDev:
			if l.PID != 111 {
				t.Errorf("port %d owned by pid %d, want 111", l.Port, l.PID)
			}
		case portAPI:
			if l.PID != 0 {
				t.Errorf("port %d claimed pid %d; nothing readable holds it", l.Port, l.PID)
			}
		}
	}
}

// The whole parent chain is read, not just the listener's own row: a dev
// server is normally a grandchild of the session that started it.
func TestEnumerateRecordsTheWholeParentChain(t *testing.T) {
	f := newProcFixture(t)
	f.listenRow(false, hexLoopbackV4, portDev, 300)
	f.process(t, 500, 400, 300, "vite", 300)
	f.process(t, 400, 300, 300, "npm")
	f.process(t, 300, 1, 300, "claude")
	root := f.write(t)

	_, parents, err := enumerateListeners(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	for pid, want := range map[int]int{500: 400, 400: 300, 300: 1} {
		if parents[pid] != want {
			t.Errorf("parents[%d] = %d, want %d", pid, parents[pid], want)
		}
	}
}

func TestParseHexAddr(t *testing.T) {
	for _, tc := range []struct {
		cell string
		addr string
		port int
		ok   bool
	}{
		{hexLoopbackV4 + ":1435", "127.0.0.1", 5173, true},
		{hexWildcardV4 + ":0BB8", "0.0.0.0", 3000, true},
		{hexRoutableV4 + ":1F90", "192.168.1.10", 8080, true},
		{hexLoopbackV6 + ":1435", "::1", 5173, true},
		{hexWildcardV6 + ":1435", "::", 5173, true},
		{"nonsense", "", 0, false},
		{"0100007:1435", "", 0, false},
	} {
		addr, port, ok := parseHexAddr(tc.cell)
		if ok != tc.ok {
			t.Errorf("parseHexAddr(%q) ok = %v, want %v", tc.cell, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if addr.String() != tc.addr || port != tc.port {
			t.Errorf("parseHexAddr(%q) = %s:%d, want %s:%d", tc.cell, addr, port, tc.addr, tc.port)
		}
	}
}

// A command name may contain spaces and parentheses, so the split is on
// the LAST `)`. Getting this wrong shifts ppid and pgrp by however many
// spaces the name has, which silently breaks attribution.
func TestParseProcStatHandlesAwkwardCommandNames(t *testing.T) {
	for _, tc := range []struct {
		line string
		comm string
		ppid int
		pgid int
	}{
		{"7 (node) S 3 4 0 0 -1 0", "node", 3, 4},
		{"7 (my dev server) S 3 4 0 0 -1 0", "my dev server", 3, 4},
		{"7 (a)b) S 3 4 0 0 -1 0", "a)b", 3, 4},
	} {
		stat, ok := parseProcStat(tc.line)
		if !ok {
			t.Fatalf("parseProcStat(%q) refused a legal line", tc.line)
		}
		if stat.comm != tc.comm || stat.ppid != tc.ppid || stat.pgid != tc.pgid {
			t.Errorf("parseProcStat(%q) = %+v, want %s/%d/%d", tc.line, stat, tc.comm, tc.ppid, tc.pgid)
		}
	}
}
