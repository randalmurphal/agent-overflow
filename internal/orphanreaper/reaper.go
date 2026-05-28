package orphanreaper

import (
	"bufio"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"time"
)

// controlFD is the inherited descriptor the parent passes to the reaper
// child: the read end of the control/death pipe. The parent keeps the
// write end, and EOF here is the parent-death signal.
const controlFD = 3

// killGracePeriod is the SIGTERM→SIGKILL window when tearing down watched
// groups. SIGTERM first lets a provider try to drop its own subtree
// cleanly; SIGKILL guarantees it's gone.
const killGracePeriod = 2 * time.Second

// RunChild is the entry point for the `__reap` subcommand. It reads
// watch/release commands from the inherited control fd and, on EOF — the
// parent died by any means (clean exit, panic, or SIGKILL, all of which
// close the parent's write end) — kills every still-watched process
// group so no provider subprocess is left orphaned. Returns when done so
// main() can exit.
func RunChild() {
	f := os.NewFile(controlFD, "orphanreaper-control")
	if f == nil {
		return
	}
	defer f.Close()
	run(f, killGroup, killGracePeriod)
}

// run is the testable core: consume commands until the pipe closes, then
// reap whatever is still watched. kill is injected so tests exercise the
// EOF→reap behaviour without signalling real process groups.
func run(r io.Reader, kill func(pgid int, sig syscall.Signal), grace time.Duration) {
	watched := make(map[int]struct{})
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		cmd, err := parseCommand(line)
		if err != nil {
			log.Printf("%v", err)
			continue
		}
		switch cmd.kind {
		case cmdWatch:
			watched[cmd.pgid] = struct{}{}
		case cmdRelease:
			delete(watched, cmd.pgid)
		}
	}
	// Scanner reports EOF as Err()==nil. A non-nil error means the pipe
	// broke abnormally — the parent is gone either way, so reap.
	if err := scanner.Err(); err != nil {
		log.Printf("orphanreaper: control read error (treating as parent death): %v", err)
	}
	reap(watched, kill, grace)
}

func reap(watched map[int]struct{}, kill func(pgid int, sig syscall.Signal), grace time.Duration) {
	if len(watched) == 0 {
		return
	}
	for pgid := range watched {
		kill(pgid, syscall.SIGTERM)
	}
	time.Sleep(grace)
	for pgid := range watched {
		kill(pgid, syscall.SIGKILL)
	}
	log.Printf("orphanreaper: parent died; reaped %d provider group(s)", len(watched))
}
