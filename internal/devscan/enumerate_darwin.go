//go:build darwin

package devscan

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The macOS enumerator. There is no /proc, and the supported way to ask
// which process holds which socket is `lsof`, which ships with the OS.
//
// Two read-only commands, both bounded:
//
//	lsof -iTCP -sTCP:LISTEN -P -n -F pcn
//	ps -eo pid=,ppid=,pgid=,comm=
//
// `-F pcn` is lsof's machine-readable form: one field per line, tagged by
// its first byte, `p` opening a process record and every later `n` naming
// a socket that process holds. `-P` and `-n` turn off port-name and host
// lookups, which is both faster and the difference between a bounded
// command and one that waits on DNS.
//
// The PARSERS are pure functions over that text, so the tests never
// execute either command — the repo's rule about never spawning anything
// in tests applies to the platform's tools as much as to a provider CLI.

// enumerateCommandTimeout bounds each read. Both commands answer in
// milliseconds on an idle machine; the ceiling is what keeps a wedged
// one from holding the scan loop.
const enumerateCommandTimeout = 3 * time.Second

// listener is one LISTEN socket, with what is known about the process
// holding it. Same shape as the Linux half.
type listener struct {
	Port int
	PID  int
	PPID int
	PGID int
	Comm string
}

// enumerateListeners returns this machine's loopback/wildcard LISTEN
// sockets and the pid → ppid map attribution walks.
//
// procRoot is unused here and named for the interface the Linux half
// defines: nothing on macOS reads a proc tree.
func enumerateListeners(_ string) ([]listener, map[int]int, error) {
	sockets, err := runEnumerator("lsof", "-iTCP", "-sTCP:LISTEN", "-P", "-n", "-F", "pcn")
	if err != nil {
		return nil, nil, fmt.Errorf("devscan: list listening sockets: %w", err)
	}
	table, err := runEnumerator("ps", "-eo", "pid=,ppid=,pgid=,comm=")
	if err != nil {
		return nil, nil, fmt.Errorf("devscan: read the process table: %w", err)
	}

	stats, parents := parsePSTable(table)
	listeners := parseLSOF(sockets)
	for i := range listeners {
		if stat, ok := stats[listeners[i].PID]; ok {
			listeners[i].PPID = stat.ppid
			listeners[i].PGID = stat.pgid
			if listeners[i].Comm == "" {
				listeners[i].Comm = stat.comm
			}
		}
	}
	return listeners, parents, nil
}

// runEnumerator executes one read-only platform tool under a deadline.
// A non-zero exit from lsof is normal — it answers 1 when it found
// nothing — so the output is used whenever there is any.
func runEnumerator(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), enumerateCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil && len(out) == 0 {
		return "", err
	}
	return string(out), nil
}

type procStat struct {
	comm string
	ppid int
	pgid int
}

// parseLSOF reads `-F pcn` output. A `p` line opens a process record and
// resets the command; every `n` line inside it is one socket.
func parseLSOF(output string) []listener {
	var listeners []listener
	pid, comm := 0, ""
	for _, line := range strings.Split(output, "\n") {
		if len(line) < 2 {
			continue
		}
		value := line[1:]
		switch line[0] {
		case 'p':
			parsed, err := strconv.Atoi(value)
			if err != nil {
				pid, comm = 0, ""
				continue
			}
			pid, comm = parsed, ""
		case 'c':
			comm = value
		case 'n':
			port, ok := parseLSOFAddress(value)
			if !ok || pid == 0 {
				continue
			}
			listeners = append(listeners, listener{Port: port, PID: pid, Comm: comm})
		}
	}
	return listeners
}

// parseLSOFAddress accepts the loopback and wildcard spellings lsof
// prints for a listening socket and refuses everything else, which is the
// same filter the Linux half applies to the socket table: a listener
// already bound to a routable address is somebody else's service.
func parseLSOFAddress(name string) (int, bool) {
	// `->` marks a connected socket. LISTEN rows never carry one, but the
	// check costs nothing and a stray row must not parse as a port.
	if strings.Contains(name, "->") {
		return 0, false
	}
	colon := strings.LastIndexByte(name, ':')
	if colon < 0 {
		return 0, false
	}
	port, err := strconv.Atoi(name[colon+1:])
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	host := strings.TrimSuffix(strings.TrimPrefix(name[:colon], "["), "]")
	if host == "*" {
		return port, true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || (!addr.IsLoopback() && !addr.IsUnspecified()) {
		return 0, false
	}
	return port, true
}

// parsePSTable reads `pid ppid pgid comm` rows. The command is last so it
// may contain spaces, which is why the split is bounded to four fields.
func parsePSTable(output string) (map[int]procStat, map[int]int) {
	stats := map[int]procStat{}
	parents := map[int]int{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		stats[pid] = procStat{comm: strings.Join(fields[3:], " "), ppid: ppid, pgid: pgid}
		parents[pid] = ppid
	}
	return stats, parents
}
