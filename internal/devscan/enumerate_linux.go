//go:build linux

package devscan

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The Linux enumerator, which is also what the Windows deployment runs:
// that ships as a WSL payload, so its backend is this file's world.
//
// Three reads, in this order, and each narrows the next:
//
//  1. /proc/net/tcp and /proc/net/tcp6 for sockets in LISTEN bound to
//     loopback or the wildcard. Anything already bound to a routable
//     address is somebody else's service, and the gateway would refuse to
//     bind its port anyway.
//  2. /proc/<pid>/fd for the socket inodes step 1 collected, so the walk
//     is bounded by the ports we care about rather than by every fd on
//     the machine.
//  3. /proc/<pid>/stat for the command name and the parent chain, read
//     only for the pids step 2 found plus their ancestors.
//
// A pid this process cannot read is SKIPPED, never an error: another
// user's processes are none of our business, and a scan that failed
// whenever the machine had one would never succeed on a shared host.

// listener is one LISTEN socket, with what is known about the process
// holding it.
type listener struct {
	Port int
	PID  int
	PPID int
	PGID int
	Comm string
}

// enumerateListeners returns this machine's loopback/wildcard LISTEN
// sockets and the pid → ppid map attribution walks.
func enumerateListeners(procRoot string) ([]listener, map[int]int, error) {
	inodes := map[uint64]int{}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		raw, err := os.ReadFile(filepath.Join(procRoot, name))
		if err != nil {
			if os.IsNotExist(err) {
				// A kernel built without IPv6 has no net/tcp6, which is a
				// configuration rather than a failure.
				continue
			}
			return nil, nil, fmt.Errorf("devscan: read %s: %w", name, err)
		}
		parseSocketTable(string(raw), inodes)
	}
	if len(inodes) == 0 {
		return nil, map[int]int{}, nil
	}

	owners := socketOwners(procRoot, inodes)
	stats := map[int]procStat{}
	listeners := make([]listener, 0, len(owners))
	for inode, port := range inodes {
		pid, ok := owners[inode]
		if !ok {
			// Nothing readable holds it. Still worth reporting: the port
			// is open, the probe decides whether it answers like a page,
			// and attribution simply finds no owner.
			listeners = append(listeners, listener{Port: port})
			continue
		}
		stat, ok := readStatCached(procRoot, pid, stats)
		if !ok {
			listeners = append(listeners, listener{Port: port, PID: pid})
			continue
		}
		listeners = append(listeners, listener{
			Port: port, PID: pid, PPID: stat.ppid, PGID: stat.pgid, Comm: stat.comm,
		})
	}

	// The parent chain has to reach past the listeners themselves: a dev
	// server is usually a grandchild of the provider session, so every
	// ancestor of every candidate is read before attribution walks.
	parents := map[int]int{}
	for _, l := range listeners {
		walkAncestors(procRoot, l.PID, stats, parents)
	}
	return listeners, parents, nil
}

// parseSocketTable reads one /proc/net/tcp* table, recording inode → port
// for every LISTEN row bound to loopback or the wildcard.
func parseSocketTable(table string, into map[uint64]int) {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		// sl local rem st tx:rx tr:tm retrnsmt uid timeout inode
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		addr, port, ok := parseHexAddr(fields[1])
		if !ok || port == 0 {
			continue
		}
		if !addr.IsLoopback() && !addr.IsUnspecified() {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			continue
		}
		into[inode] = port
	}
}

// parseHexAddr decodes a `<address>:<port>` cell from /proc/net/tcp*.
//
// The kernel prints the address as native-endian 32-bit words, so each
// four-byte group comes out byte-reversed on every platform this app
// ships on (all of them little-endian). Reversing per group rather than
// wholesale is what makes the IPv6 form come out right.
func parseHexAddr(cell string) (netip.Addr, int, bool) {
	host, portText, ok := strings.Cut(cell, ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	port64, err := strconv.ParseUint(portText, 16, 32)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	raw, err := hex.DecodeString(host)
	if err != nil || len(raw)%4 != 0 || len(raw) == 0 {
		return netip.Addr{}, 0, false
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+3] = raw[i+3], raw[i]
		raw[i+1], raw[i+2] = raw[i+2], raw[i+1]
	}
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.Addr{}, 0, false
	}
	return addr.Unmap(), int(port64), true
}

// socketOwners walks /proc/<pid>/fd once, looking only for the inodes the
// socket tables named.
func socketOwners(procRoot string, inodes map[uint64]int) map[uint64]int {
	owners := make(map[uint64]int, len(inodes))
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return owners
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, entry.Name(), "fd"))
		if err != nil {
			// Another user's process, or one that exited mid-walk.
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(procRoot, entry.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := socketInode(target)
			if !ok {
				continue
			}
			if _, wanted := inodes[inode]; !wanted {
				continue
			}
			if _, claimed := owners[inode]; !claimed {
				owners[inode] = pid
			}
		}
		if len(owners) == len(inodes) {
			break
		}
	}
	return owners
}

// socketInode reads the inode out of a `socket:[12345]` fd link target.
func socketInode(target string) (uint64, bool) {
	const prefix = "socket:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	inode, err := strconv.ParseUint(target[len(prefix):len(target)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return inode, true
}

type procStat struct {
	comm string
	ppid int
	pgid int
}

// readStatCached reads /proc/<pid>/stat once per scan.
func readStatCached(procRoot string, pid int, cache map[int]procStat) (procStat, bool) {
	if pid <= 0 {
		return procStat{}, false
	}
	if stat, ok := cache[pid]; ok {
		return stat, stat.ppid != 0 || stat.comm != ""
	}
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		cache[pid] = procStat{}
		return procStat{}, false
	}
	stat, ok := parseProcStat(string(raw))
	if !ok {
		cache[pid] = procStat{}
		return procStat{}, false
	}
	cache[pid] = stat
	return stat, true
}

// parseProcStat reads comm, ppid and pgrp out of a /proc/<pid>/stat line.
//
// The command name is in parentheses and MAY CONTAIN SPACES AND
// PARENTHESES — `(my server)` and `(a)b)` are both legal — so the split
// is on the LAST `)` rather than on whitespace. Everything after it is
// space-separated and positional: state, ppid, pgrp.
func parseProcStat(line string) (procStat, bool) {
	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open < 0 || close < open {
		return procStat{}, false
	}
	rest := strings.Fields(line[close+1:])
	if len(rest) < 3 {
		return procStat{}, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return procStat{}, false
	}
	pgid, err := strconv.Atoi(rest[2])
	if err != nil {
		return procStat{}, false
	}
	return procStat{comm: line[open+1 : close], ppid: ppid, pgid: pgid}, true
}

// walkAncestors records pid → ppid for a listener and every ancestor,
// stopping at init, at an unreadable pid, or at a pid already recorded.
// The depth bound is what keeps a corrupt or hand-built tree with a cycle
// in it from spinning.
func walkAncestors(procRoot string, pid int, stats map[int]procStat, parents map[int]int) {
	for depth := 0; depth < 64 && pid > 1; depth++ {
		if _, seen := parents[pid]; seen {
			return
		}
		stat, ok := readStatCached(procRoot, pid, stats)
		if !ok {
			return
		}
		parents[pid] = stat.ppid
		pid = stat.ppid
	}
}
