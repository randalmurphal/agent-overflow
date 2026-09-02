package devscan

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// DevServer is one row of the machine's dev-server list, as the frontend
// receives it. The JSON names are the wire contract; do not rename one
// without the client half.
type DevServer struct {
	// Port is the port the dev server listens on, and also the port this
	// machine's preview listener binds. Same number on both sides
	// deliberately: a dev server's absolute URLs, its `<base href>` and
	// every HMR client that derives its socket from `location.host` keep
	// working unmodified only if the port does not move.
	Port int `json:"port"`

	// PID is the process holding the socket, or 0 when this scan could
	// not read it (a process owned by another user, or one that exited
	// between the socket table and the fd walk).
	PID int `json:"pid,omitempty"`

	// Process is that process's command name, for the person reading the
	// list. Never used for attribution.
	Process string `json:"process,omitempty"`

	// ThreadID is the thread whose provider session or terminal this
	// listener descends from, or "" when nothing claimed it.
	ThreadID string `json:"threadId,omitempty"`

	// Allowed is true when this port is in the machine's preview set: a
	// preview listener exists for it and a URL can be minted.
	Allowed bool `json:"allowed"`

	// Source says WHY it is in the list, and the three values are three
	// different affordances on screen:
	//
	//   allowed    — the owner named it in network.previewPorts.
	//   attributed — a thread owns it and nobody named it; the link is
	//                live for as long as the process is.
	//   seen       — listening and answering like a page, owned by
	//                nothing this app started. The candidate list the
	//                "Allow port" action draws from.
	//
	// "allowed" means "in the persisted list"; "attributed" means "shared
	// only by attribution". A hand-named port that a thread also owns is
	// ALLOWED — it carries ThreadID, PID and Process all the same, but
	// calling it attributed would hide the persisted entry from the
	// settings screen and leave no way to stop sharing it.
	Source string `json:"source"`

	// Listening is false in exactly two cases, and the row exists in both
	// so the URL does not vanish under the person reading it: during the
	// grace after an attributed listener's socket disappeared (a dev
	// server restarting), and for an allowed port nothing is serving.
	Listening bool `json:"listening"`

	// Note is a sentence about why this port is not proxied even though
	// it is in the set. Written by the gateway, never by the scan.
	Note string `json:"note,omitempty"`
}

// The values DevServer.Source takes.
const (
	SourceAttributed = "attributed"
	SourceAllowed    = "allowed"
	SourceSeen       = "seen"
)

// DevServerList is the whole per-backend answer.
type DevServerList struct {
	// Servers is never null and is sorted by port, so a client diffing
	// two frames compares like with like.
	Servers []DevServer `json:"servers"`

	// PreviewHost is the authority a preview URL on this machine names:
	// the tailnet DNS name, else the LAN IP, else "" when this backend has
	// no address to share a preview on at all. The client renders the
	// third case as its own sentence rather than as a broken link.
	PreviewHost string `json:"previewHost"`
}

// Owner is one process this app started that a listener can be traced
// back to: a thread's provider session, or one of its terminals.
type Owner struct {
	ThreadID string
	// PID is the process this app spawned.
	PID int
	// PGID is that process's group. Both spawn paths call
	// procutil.ConfigureGroup, so the child leads its own group and this
	// equals PID — but it is carried separately because the MATCH is a
	// different one: a dev server that daemonised has left the ancestor
	// chain and is still in the group.
	PGID int
}

// ErrUnsupported is what a platform with no enumerator answers. Returned
// verbatim rather than folded into an empty list: a caller that cannot
// discover anything here has to be able to say so, and "no dev servers"
// and "this build cannot look" are different sentences on screen.
var ErrUnsupported = errors.New("devscan: dev-server discovery is not supported on this platform")

// attributedGrace is how long an attributed port stays in the preview set
// after its listener disappears. A dev server restarting (a config edit,
// a crash-and-restart under a watcher) is the common case, and tearing the
// URL down for the two seconds that takes would make every preview link
// unreliable in exactly the situation people use one.
const attributedGrace = 60 * time.Second

// maxProbesPerScan bounds how many candidates one scan will probe. The
// verdict cache absorbs the steady state, so this only bites on a machine
// with a great many fresh listeners — where spending seconds probing all
// of them would make the scan itself the problem.
const maxProbesPerScan = 32

// Scanner produces the machine's dev-server list. Safe for concurrent
// use; one belongs to the App.
type Scanner struct {
	// procRoot is the directory the Linux enumerator reads, "/proc" in
	// production and a fixture tree under a temp dir in tests. Ignored by
	// the enumerators that shell out.
	procRoot string

	probe *prober
	now   func() time.Time

	mu sync.Mutex
	// grace remembers attributed ports whose listener has gone, so the
	// preview set does not collapse under a dev-server restart. Keyed by
	// port; bounded by the number of ports this machine ever attributed,
	// and swept on every scan.
	grace map[int]graceEntry
}

type graceEntry struct {
	threadID string
	process  string
	pid      int
	until    time.Time
}

// New returns a Scanner reading the real /proc and dialling the real
// loopback.
func New() *Scanner { return newScanner("/proc", time.Now) }

// newScanner is the injected form both New and the tests build.
func newScanner(procRoot string, now func() time.Time) *Scanner {
	return &Scanner{
		procRoot: procRoot,
		probe:    newProber(now),
		now:      now,
		grace:    make(map[int]graceEntry),
	}
}

// Scan enumerates this machine's listening sockets, attributes what it
// can to owners, probes the candidates and returns the sorted list.
//
// allowed is the owner's hand-named port set (`network.previewPorts`).
// Those ports appear whatever is or is not listening on them, because the
// person said so and a row that vanished would read as the setting having
// been lost.
func (s *Scanner) Scan(ctx context.Context, owners []Owner, allowed []int) ([]DevServer, error) {
	listeners, parents, err := enumerateListeners(s.procRoot)
	if err != nil {
		return nil, err
	}

	allowedSet := make(map[int]struct{}, len(allowed))
	for _, port := range allowed {
		allowedSet[port] = struct{}{}
	}

	// One row per PORT, not per socket: a dev server bound to both
	// loopback families is two rows in the kernel's table and one thing on
	// screen. The first socket seen wins the process fields, and a later
	// one only ever adds attribution.
	rows := make(map[int]DevServer, len(listeners))
	for _, listener := range listeners {
		row, seen := rows[listener.Port]
		if !seen {
			row = DevServer{
				Port:      listener.Port,
				PID:       listener.PID,
				Process:   listener.Comm,
				Listening: true,
				Source:    SourceSeen,
			}
		}
		if row.ThreadID == "" {
			if threadID, ok := attribute(listener, owners, parents); ok {
				row.ThreadID = threadID
				row.PID = listener.PID
				row.Process = listener.Comm
			}
		}
		rows[listener.Port] = row
	}

	probes := 0
	servers := make([]DevServer, 0, len(rows)+len(allowed))
	for port, row := range rows {
		_, handNamed := allowedSet[port]
		switch {
		case handNamed:
			// The owner named this port. It is published whatever it
			// answers — the probe is a filter on candidates nobody
			// chose, not a second opinion on a choice already made.
			//
			// SourceAllowed wins over attribution, always. Source says
			// WHERE the row came from, and a hand-named port came from
			// the persisted list even when a thread also owns it. The
			// thread is still named on the row; what would be lost by
			// calling it attributed is the only handle the settings
			// screen has for taking it back out of the list.
			row.Source = SourceAllowed
			row.Allowed = true
		default:
			if probes >= maxProbesPerScan {
				continue
			}
			probes++
			if !s.probe.answersLikeAPage(ctx, port, row.PID) {
				continue
			}
			if row.ThreadID != "" {
				row.Source = SourceAttributed
				row.Allowed = true
			}
		}
		servers = append(servers, row)
	}

	servers = s.applyGrace(servers)
	servers = appendMissingAllowed(servers, allowed)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Port < servers[j].Port })
	return servers, nil
}

// applyGrace records the attributed ports this scan saw and re-adds the
// ones whose listener has gone but whose deadline has not passed.
func (s *Scanner) applyGrace(servers []DevServer) []DevServer {
	now := s.now()
	present := make(map[int]struct{}, len(servers))

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range servers {
		present[row.Port] = struct{}{}
		if row.ThreadID == "" {
			// Only the attributed half has a grace. A "seen" port that
			// stopped listening is not a preview anybody lost, and an
			// allowed one is in the set on the owner's say-so already.
			delete(s.grace, row.Port)
			continue
		}
		s.grace[row.Port] = graceEntry{
			threadID: row.ThreadID,
			process:  row.Process,
			pid:      row.PID,
			until:    now.Add(attributedGrace),
		}
	}
	for port, entry := range s.grace {
		if !now.Before(entry.until) {
			delete(s.grace, port)
			continue
		}
		if _, live := present[port]; live {
			continue
		}
		servers = append(servers, DevServer{
			Port:      port,
			PID:       entry.pid,
			Process:   entry.process,
			ThreadID:  entry.threadID,
			Allowed:   true,
			Source:    SourceAttributed,
			Listening: false,
		})
	}
	return servers
}

// appendMissingAllowed adds a row for every hand-named port nothing is
// listening on, so the setting is visible on screen rather than only in
// the file.
func appendMissingAllowed(servers []DevServer, allowed []int) []DevServer {
	present := make(map[int]struct{}, len(servers))
	for _, row := range servers {
		present[row.Port] = struct{}{}
	}
	for _, port := range allowed {
		if _, ok := present[port]; ok {
			continue
		}
		present[port] = struct{}{}
		servers = append(servers, DevServer{
			Port:      port,
			Allowed:   true,
			Source:    SourceAllowed,
			Listening: false,
		})
	}
	return servers
}

// PreviewPorts returns the ports a gateway should hold a listener on: the
// preview set, which is the attributed half of a scan plus the owner's
// hand-named ports. Sorted and deduplicated.
func PreviewPorts(servers []DevServer) []int {
	ports := make([]int, 0, len(servers))
	for _, row := range servers {
		if !row.Allowed {
			continue
		}
		ports = append(ports, row.Port)
	}
	sort.Ints(ports)
	return ports
}
