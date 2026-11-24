package app

import (
	"cmp"
	"context"
	"errors"
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A spawn on a paired computer in a listing: the rows that computer answered,
// nested under the spawn's row, and what that row reads from the answer.

// threadAgentPeerMode is how a listing reads a spawn's rows on a paired
// computer.
type threadAgentPeerMode int

const (
	// threadAgentPeersCached nests the last answer and asks again in the
	// background: a pane's read.
	threadAgentPeersCached threadAgentPeerMode = iota
	// threadAgentPeersLive asks each computer now, concurrently: an
	// agent_rows answer for another computer, which asked off its pane's
	// path.
	threadAgentPeersLive
)

// threadAgentRemoteSlot is a spawn on a paired computer in a listing: its
// row's index and the depth its nested rows start at.
type threadAgentRemoteSlot struct {
	index int
	depth int
	spawn store.ThreadRequest
	// shown is the request the row shows: the spawn, or a send of a later
	// run of its caller's there (store.ThreadAgentRequest.Node).
	shown store.ThreadRequest
	// ended says the spawn's run ended (remoteSpawnEnded): it nests only
	// the rows its run left there, none of the work someone else's run
	// does since.
	ended bool
}

// remoteAgentAnswers reads each remote spawn's rows: the last answer, with
// another asked in the background, or every computer now, concurrently,
// recorded as any answer is and nested as the kept one, so a computer that
// does not answer now shows its last answer either way. A spawn whose
// computer is no longer paired is asked nothing and answers that it is not.
func (a *App) remoteAgentAnswers(ctx context.Context, remote []threadAgentRemoteSlot, maxDepth int, peers threadAgentPeerMode) []threadAgentPeerAnswer {
	answers := make([]threadAgentPeerAnswer, len(remote))
	pairing := map[string]error{}
	ask := make([]int, 0, len(remote))
	for i, slot := range remote {
		computer := slot.spawn.TargetComputerID
		err, seen := pairing[computer]
		if !seen {
			var paired bool
			if paired, err = a.peerPaired(computer); err == nil && !paired {
				err = errThreadAgentPeerUnpaired
			}
			pairing[computer] = err
		}
		if err != nil {
			answers[i] = threadAgentPeerAnswer{err: err}
			continue
		}
		ask = append(ask, i)
	}
	if peers == threadAgentPeersLive {
		runBounded(len(ask), threadAgentPeerListsInFlight, func(n int) {
			i := ask[n]
			call, cancel := context.WithTimeout(ctx, a.threadAgentPeerServeBound())
			defer cancel()
			// The levels its rows may still use, none at the bound: there the
			// answer is the thread's state alone, and that computer asks
			// none further on.
			depth := maxDepth - remote[i].depth
			answers[i] = a.readPeerAgentRows(call, remote[i].spawn, depth)
		})
		return answers
	}
	for _, i := range ask {
		if answer, ok := a.cachedPeerAgentRows(remote[i].spawn); ok {
			answers[i] = answer
		}
		a.refreshPairedPeerAgentRows(remote[i].spawn, threadAgentPeerRefreshEvery)
	}
	return answers
}

// nestPeerAgentRows renders a paired computer's answer as the rows nested
// under its spawn's row, named by that computer, deeper than the bound left
// out with their parent marked. A computer that did not answer leaves the
// reason on the spawn's row, and the state its last answer gave.
func (a *App) nestPeerAgentRows(parent *ThreadAgentRow, slot threadAgentRemoteSlot, answer threadAgentPeerAnswer, maxDepth int) ([]ThreadAgentRow, []threadAgentEntry) {
	// That computer says why the thread is not at rest, and whether it is
	// open; one with only open work below it there waits on it, as its
	// row's nested_open says once recorded. A failed call is no news of
	// rest: the last answer stands, which a refusal clears
	// (noteAnswerLocked). It says where the thread is placed and what it
	// waits on there too. A send of a later run in the spawn's place keeps
	// the state its own request reads (threadAgentView.row).
	if slot.shown.Token == slot.spawn.Token {
		switch open := answer.listing.Open; {
		case slot.spawn.RunEnded():
			// Its run ended: what the thread does there now is not this row's.
		case answer.listing.Worker != "":
			parent.Worker = answer.listing.Worker
		case open != nil && *open && parent.Worker == "":
			parent.Worker = workerWaiting
		}
		peerSpawnReport(parent, slot.spawn, answer)
		if parent.Worker == workerBlocked || parent.State == threadtools.RequestBlocked {
			a.peerSpawnBlocked(parent, answer.listing)
		}
		if parent.open() && parent.BlockedOn == "" {
			parent.Activity = answer.listing.Activity
		}
	}
	if answer.err != nil {
		parent.NestedUnavailable = threadAgentPeerUnavailable(parent.Computer, answer.err)
		return nil, nil
	}
	if parent.Provider == "" {
		parent.Provider = answer.listing.ThreadProvider
	}
	if !slot.ended && slot.depth >= maxDepth && answer.listing.Hidden {
		// The thread's state alone, for a thread with rows past the bound.
		parent.NestedHidden = true
	}
	self, _ := a.backendIdentity()
	rows := make([]ThreadAgentRow, 0, len(answer.listing.Rows))
	entries := make([]threadAgentEntry, 0, len(answer.listing.Rows))
	// Under a spawn whose run ended only its run's rows nest, every one done
	// by then: an open row there, and the rows under it, are work someone
	// else started since (doneRowSQL reads a local spawn the same way).
	cut := -1
	for _, row := range answer.listing.Rows {
		if slot.ended {
			if cut >= 0 && row.Depth > cut {
				continue
			}
			cut = -1
			if row.open() {
				cut = row.Depth
				continue
			}
		}
		row.Depth += slot.depth
		if row.Depth >= maxDepth {
			if slot.ended {
				continue
			}
			if row.Depth == slot.depth {
				parent.NestedHidden = true
			}
			for i := len(rows) - 1; i >= 0; i-- {
				if rows[i].Depth == row.Depth-1 {
					rows[i].NestedHidden = true
					break
				}
			}
			continue
		}
		row.OwnerComputerID = slot.spawn.TargetComputerID
		// The placement stays the one the computer that holds the thread
		// reported. A prompt routed back to that computer's spawner is
		// routed to a thread there, not here, and named with that computer.
		if row.BlockedRoutedHere {
			row.BlockedRoutedTo, row.BlockedRoutedHere = row.BlockedRoutedTo+" on "+parent.Computer, false
		}
		switch row.ComputerID {
		case "":
			// A thread on that computer.
			row.ComputerID, row.Computer = slot.spawn.TargetComputerID, parent.Computer
		case self:
			// A thread that computer reached back here on.
			row.ComputerID, row.Computer = "", ""
		}
		rows = append(rows, row)
		entries = append(entries, threadAgentEntry{
			peer: slot.spawn.TargetComputerID, peerSource: slot.spawn.CallerThreadID, peerRoot: slot.spawn.TargetThreadID,
		})
	}
	return rows, entries
}

// peerSpawnReport sets what a paired computer's spawn did and where its
// thread is placed there, as that computer's last answer reported it: the
// one projection of a paired spawn's counts, placement and archive state,
// for its open row and its Past row alike.
func peerSpawnReport(row *ThreadAgentRow, spawn store.ThreadRequest, answer threadAgentPeerAnswer) {
	row.runFigures(spawn, peerFigures(answer.listing))
	row.Placement, row.Archived = answer.listing.Placement, answer.listing.Archived
}

// peerSpawnBlocked sets what a blocked paired spawn's thread waits on, as
// that computer's answer reports it; a row the answer says nothing for
// keeps what the request's poll said. A prompt that computer routed back
// to this spawn's holder is named as this computer names the holder.
func (a *App) peerSpawnBlocked(row *ThreadAgentRow, listing ThreadAgentRows) {
	if listing.BlockedOn == "" {
		return
	}
	row.BlockedOn, row.BlockedDetail, row.BlockedRoutedTo = listing.BlockedOn, listing.BlockedDetail, listing.BlockedRoutedTo
	row.BlockedRoutedHere = listing.RoutedToAsker
	if listing.RoutedToAsker {
		row.BlockedRoutedTo = a.routedHereTitle(row.ComputerID, row.ThreadID, row.CallerThreadID, listing.BlockedRoutedTo)
	}
}

// routedHereTitle names the thread here that holds the prompt the paired
// computer computerID routed back here for its thread workerID: the thread
// that prompt's wake goes to, the caller now of the spawn from here that
// made the worker (store.RemoteSpawnLinkTo, store.RemotePromptHolder, as
// deliverRemotePrompt reads it), which a handoff moves. While no thread
// here can take it, which sends it back, it names named, the thread that
// computer reported, by its title here; and the name that computer gave
// when no title can be read, so the row still reads as routed rather than
// the person's.
func (a *App) routedHereTitle(computerID, workerID, named, reported string) string {
	spawn, found, err := a.store.RemoteSpawnLinkTo(computerID, workerID)
	if err == nil && found {
		var title string
		_, title, found, err = a.store.RemotePromptHolder(spawn.Token)
		if err == nil && found && title != "" {
			return title
		}
	}
	if err != nil {
		log.Printf("agent rows: read the thread here that holds the prompt of %s on %s: %v", workerID, computerID, err)
	}
	thread, err := a.store.GetThread(named)
	if err != nil {
		log.Printf("agent rows: read the title of %s, whose worker's prompt came back to it: %v", named, err)
		return cmp.Or(reported, named)
	}
	return cmp.Or(thread.Title, reported, named)
}

// pastPeerSpawnCounts sets the counts of the paired spawns on a page of
// Past rows from each one's last answer, and asks again in the background
// for any whose answer is missing or old, a page's worth at most; each
// answer that arrives frames its row. A spawn whose computer did not
// answer, or is no longer paired, says why (NestedUnavailable).
func (a *App) pastPeerSpawnCounts(rows []ThreadAgentRow, requests []store.ThreadAgentRequest) {
	for i := range rows {
		spawn := requests[i].ThreadRequest
		if spawn.Kind != store.ThreadRequestSpawn || spawn.TargetComputerID == "" || spawn.TargetThreadID == "" {
			continue
		}
		paired, err := a.peerPaired(spawn.TargetComputerID)
		if err == nil && !paired {
			err = errThreadAgentPeerUnpaired
		}
		if err != nil {
			rows[i].NestedUnavailable = threadAgentPeerUnavailable(rows[i].Computer, err)
			continue
		}
		answer, ok := a.cachedPeerAgentRows(spawn)
		if ok {
			peerSpawnReport(&rows[i], spawn, answer)
			if answer.err != nil {
				rows[i].NestedUnavailable = threadAgentPeerUnavailable(rows[i].Computer, answer.err)
			}
		}
		a.refreshPairedPeerAgentRows(spawn, threadAgentPeerRefreshEvery)
	}
}

// threadAgentPeerUnavailable words, for the tray, why a spawn on a paired
// computer shows none of that thread's own agents. The error itself goes to
// the log.
func threadAgentPeerUnavailable(computer string, err error) string {
	if computer == "" {
		computer = "That computer"
	}
	if errors.Is(err, errThreadAgentPeerOlder) {
		return computer + " runs an older version: its agents are not shown or stopped from here"
	}
	if errors.Is(err, errThreadAgentPeerUnpaired) {
		return computer + " is no longer paired"
	}
	log.Printf("agent rows: %s did not list its agents: %v", computer, err)
	return computer + " did not answer; its agents are not shown"
}
