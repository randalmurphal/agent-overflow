package threadtools

import (
	"context"
	"strings"
	"testing"
)

const (
	localThreadID  = "11111111-1111-4111-8111-111111111111"
	remoteThreadID = "22222222-2222-4222-8222-222222222222"
	twinThreadID   = "11111111-2222-4222-8222-222222222222"
)

type pair struct {
	local  *fakeApp
	remote *fakeApp
	peer   *serverPeer
	server *Server
}

// newPair wires one computer to another through that other computer's own
// Server, so a cross-computer test exercises the real destination handler
// answering in its own shape.
func newPair(t *testing.T) *pair {
	t.Helper()
	local := newFakeApp("Laptop")
	remote := newFakeApp("Studio")
	studio := Computer{ID: "studio", Name: "Studio"}
	local.computers = []Computer{studio}
	peer := &serverPeer{computer: studio, server: New(remote), caller: localCaller()}
	local.peers[studio.ID] = peer
	return &pair{local: local, remote: remote, peer: peer, server: New(local)}
}

func (p *pair) session() *session {
	return &session{app: p.local, caller: localCaller(), computers: p.local.computers}
}

func soloSession(app *fakeApp) *session {
	return &session{app: app, caller: localCaller()}
}

// TestFullIdResolvesLocallyWithoutFanningOut: a full id is a complete
// address, so a local hit is the whole answer and no peer is disturbed.
func TestFullIdResolvesLocallyWithoutFanningOut(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})

	target, err := p.session().resolve(context.Background(), localThreadID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !target.Local || target.ThreadID != localThreadID {
		t.Fatalf("target = %+v, want the local thread", target)
	}
	if p.peer.resolves != 0 {
		t.Fatalf("the peer was asked %d times about a full id that matched locally", p.peer.resolves)
	}
}

// TestFullIdFansOutOnALocalMiss and comes back stamped with the computer
// that holds it.
func TestFullIdFansOutOnALocalMiss(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})

	target, err := p.session().resolve(context.Background(), remoteThreadID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.Local || target.ComputerID != "studio" || target.Computer != "Studio" {
		t.Fatalf("target = %+v, want the thread on Studio", target)
	}
	if p.peer.resolves != 1 {
		t.Fatalf("the peer was asked %d times, want once", p.peer.resolves)
	}
}

// TestPrefixAlwaysFansOut: an ambiguity across computers must be detected,
// not masked by a local match.
func TestPrefixAlwaysFansOut(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.remote.addThread(Thread{ID: twinThreadID, Title: "Twin on Studio"})

	if _, err := p.session().resolve(context.Background(), "1111", ""); err == nil {
		t.Fatal("a prefix shorter than the minimum was accepted")
	}
	target, err := p.session().resolve(context.Background(), "11111111-2", "")
	if err != nil {
		t.Fatalf("an unambiguous prefix was refused: %v", err)
	}
	if target.ThreadID != twinThreadID || target.ComputerID != "studio" {
		t.Fatalf("target = %+v, want the Studio twin", target)
	}

	_, err = p.session().resolve(context.Background(), "111111", "")
	code, message := publicMessage(err)
	if code != CodeAmbiguous {
		t.Fatalf("code = %q (%s), want %s", code, message, CodeAmbiguous)
	}
	if !strings.Contains(message, "Studio") || !strings.Contains(message, localThreadID) {
		t.Fatalf("the ambiguity does not list both candidates with their computers: %s", message)
	}
}

// TestPrefixMatchWithASilentComputerIsPartial: the match stands and the
// note says which computer was not asked.
func TestPrefixMatchWithASilentComputerIsPartial(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.local.peers["studio"] = brokenPeer{computer: Computer{ID: "studio", Name: "Studio"}, err: publicf(CodeUnreachable, "Studio is offline.")}

	target, err := p.session().resolve(context.Background(), "111111", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(target.Partial) != 1 || target.Partial[0].Name != "Studio" {
		t.Fatalf("partial = %+v, want Studio", target.Partial)
	}
	if note := partialNote(target.Partial); !strings.Contains(note, "Studio did not answer") {
		t.Fatalf("partial note = %q", note)
	}
}

// TestMissWithASilentComputerIsIncomplete, never a confident not found.
func TestMissWithASilentComputerIsIncomplete(t *testing.T) {
	p := newPair(t)
	p.local.peers["studio"] = brokenPeer{computer: Computer{ID: "studio", Name: "Studio"}, err: publicf(CodeUnreachable, "Studio is offline.")}

	_, err := p.session().resolve(context.Background(), remoteThreadID, "")
	code, message := publicMessage(err)
	if code != CodeResolutionIncomplete {
		t.Fatalf("code = %q (%s), want %s", code, message, CodeResolutionIncomplete)
	}
	if !strings.Contains(message, "Studio did not answer") {
		t.Fatalf("the refusal does not name the silent computer: %s", message)
	}
}

// TestMissEverywhereIsNotFound when every computer answered.
func TestMissEverywhereIsNotFound(t *testing.T) {
	p := newPair(t)
	_, err := p.session().resolve(context.Background(), remoteThreadID, "")
	code, message := publicMessage(err)
	if code != CodeNotFound {
		t.Fatalf("code = %q (%s), want %s", code, message, CodeNotFound)
	}
	if !strings.Contains(message, "Studio") || !strings.Contains(message, "thread_search") {
		t.Fatalf("the refusal should name the computers searched and how to find the right id: %s", message)
	}
}

// TestOneMoveIsFollowed: the computer that moved a thread away records the
// new owner and answers with it instead of a miss.
func TestOneMoveIsFollowed(t *testing.T) {
	p := newPair(t)
	p.local.movedRefs = []string{remoteThreadID}
	p.local.movedTo, p.local.movedToName = "studio", "Studio"
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Moved"})

	target, err := p.session().resolve(context.Background(), remoteThreadID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.ComputerID != "studio" || target.Local {
		t.Fatalf("target = %+v, want the new owner", target)
	}
}

// TestMoveToAnUnpairedComputerIsNotFound with the destination named, since
// nothing can be done about it through these tools.
func TestMoveToAnUnpairedComputerIsNotFound(t *testing.T) {
	p := newPair(t)
	p.local.movedRefs = []string{remoteThreadID}
	p.local.movedTo, p.local.movedToName = "desktop", "Desktop"

	_, err := p.session().resolve(context.Background(), remoteThreadID, "")
	code, message := publicMessage(err)
	if code != CodeNotFound || !strings.Contains(message, "Desktop") {
		t.Fatalf("code = %q, message = %q", code, message)
	}
}

// TestHintNarrowsToOneComputer and skips the fan-out entirely.
func TestHintNarrowsToOneComputer(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.remote.addThread(Thread{ID: twinThreadID, Title: "Twin"})

	target, err := p.session().resolve(context.Background(), "111111", "studio")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.ThreadID != twinThreadID {
		t.Fatalf("target = %+v, want the Studio thread", target)
	}
	if p.peer.resolves != 1 {
		t.Fatalf("the peer was asked %d times, want once", p.peer.resolves)
	}
}

// TestUnknownHintIsRefusedWithTheComputersThatExist.
func TestUnknownHintIsRefusedWithTheComputersThatExist(t *testing.T) {
	p := newPair(t)
	_, err := p.session().resolve(context.Background(), localThreadID, "nowhere")
	code, message := publicMessage(err)
	if code != CodeInvalidRequest || !strings.Contains(message, "Studio") {
		t.Fatalf("code = %q, message = %q", code, message)
	}
}

// TestSoloResolutionIsLocalOnly: with no paired computers nothing fans out
// and the refusal says nothing about other computers.
func TestSoloResolutionIsLocalOnly(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "Local"})
	c := soloSession(app)

	target, err := c.resolve(context.Background(), localThreadID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target.ComputerID != "" || target.Computer != "" {
		t.Fatalf("a single-computer result must carry no computer fields: %+v", target)
	}
	_, err = c.resolve(context.Background(), remoteThreadID, "")
	_, message := publicMessage(err)
	if !strings.Contains(message, "on this computer.") {
		t.Fatalf("refusal = %q, want it to name only this computer", message)
	}
}

// TestLocalAmbiguityIsRefusedWithoutPairing too: a prefix that matches two
// local threads is the same mistake.
func TestLocalAmbiguityIsRefusedWithoutPairing(t *testing.T) {
	app := newFakeApp("Laptop")
	app.addThread(Thread{ID: localThreadID, Title: "One"})
	app.addThread(Thread{ID: twinThreadID, Title: "Two"})

	_, err := soloSession(app).resolve(context.Background(), "1111", "")
	if code, _ := publicMessage(err); code != CodeInvalidRequest {
		t.Fatalf("a too-short prefix should be refused as an invalid request, got %v", err)
	}
	_, err = soloSession(app).resolve(context.Background(), "111111", "")
	code, _ := publicMessage(err)
	if code != CodeAmbiguous {
		t.Fatalf("code = %q, want %s", code, CodeAmbiguous)
	}
}
