package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

func (p *peerFaults) ThreadToolExportChunk(ctx context.Context, exportID string, offset int64) (RemoteArtifactChunk, error) {
	p.exportMu.Lock()
	p.exportAsked = append(p.exportAsked, offset)
	p.exportMu.Unlock()
	chunk, err := p.App.ThreadToolExportChunk(ctx, exportID, offset)
	if err == nil && p.exportChunk != nil {
		p.exportChunk(ctx, offset, &chunk)
	}
	return chunk, err
}

func (p *peerFaults) ThreadToolResolve(ctx context.Context, resolve ThreadPeerResolve) (threadtools.Resolution, error) {
	if hold := p.resolveHold.Load(); hold != nil {
		p.resolveHeld <- struct{}{}
		select {
		case <-*hold:
		case <-ctx.Done():
			return threadtools.Resolution{}, ctx.Err()
		}
	}
	return p.App.ThreadToolResolve(ctx, resolve)
}

// newExportPair is a reach pair whose destination serves through
// peerFaults, holding one thread whose tool output is size bytes, so its
// whole export is a little more.
func newExportPair(t *testing.T, size int) (*reachPair, *peerFaults, string) {
	t.Helper()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	remote := reachThread(t, pair.dest, "eeeeeeee-0000-4000-8000-000000000001", "Exported")
	if err := pair.dest.store.InsertTurn(store.Turn{TurnID: remote.ID + "-turn", ThreadID: remote.ID, StartedAt: 1_000}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := pair.dest.store.UpsertItem(store.Item{
		ID: "output", ThreadID: remote.ID, Kind: "tool_call", Role: "assistant", Status: "completed",
		ToolName: "Bash", Summary: "Bash", PayloadID: "output-payload", PayloadKind: "tool_result",
		CreatedAt: 1_000, UpdatedAt: 1_000,
	}, &store.Payload{ID: "output-payload", Kind: "tool_result", Data: []byte(strings.Repeat("x", size)), CreatedAt: 1_000}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	return pair, faults, remote.ID
}

func exportToFile(pair *reachPair, t *testing.T, threadID string) (map[string]any, error) {
	return pair.callRaw(t, "thread_show", `{"thread_id":"`+threadID+`","window":"all","to_file":true,"include":["all"]}`)
}

// onlyPeerExport is the one export the destination rendered for the source.
func onlyPeerExport(t *testing.T, app *App) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(app.configDir, threadExportDirName))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), threadPeerExportPrefix) {
			found = append(found, entry.Name())
		}
	}
	if len(found) != 1 {
		t.Fatalf("peer exports %q, want one", found)
	}
	return found[0]
}

// TestAnExportCopyHoldsOneConnectionForAllItsChunks: the chunks of one copy
// share one connection however busy the pairing is around them. Between
// the first two chunks the source's pool is filled past what it keeps
// idle, so a copy that gave its connection back after each chunk would
// lose it there and carry on over another.
func TestAnExportCopyHoldsOneConnectionForAllItsChunks(t *testing.T) {
	t.Parallel()
	pair, faults, threadID := newExportPair(t, 2*remoteArtifactChunkBytes)
	const busy = 8 // more than the pool keeps idle (maxIdlePeerConns)
	faults.resolveHeld = make(chan struct{}, busy)
	var mu sync.Mutex
	var conns []*transport.ConnState
	faults.exportChunk = func(ctx context.Context, offset int64, _ *RemoteArtifactChunk) {
		mu.Lock()
		conns = append(conns, transport.ConnStateFromContext(ctx))
		mu.Unlock()
		if offset != 0 {
			return
		}
		hold := make(chan struct{})
		faults.resolveHold.Store(&hold)
		var wg sync.WaitGroup
		for range busy {
			wg.Go(func() {
				var resolution threadtools.Resolution
				err := pair.manager.CallThreadPeer(ctx, pair.computer, "ThreadToolResolve", &resolution, ThreadPeerResolve{Ref: "eeeeee"})
				var answered *rpcclient.Error
				if err != nil && !errors.As(err, &answered) {
					t.Errorf("a call beside the copy: %v", err)
				}
			})
		}
		for range busy {
			<-faults.resolveHeld
		}
		faults.resolveHold.Store(nil)
		close(hold)
		wg.Wait()
	}

	if _, err := exportToFile(pair, t, threadID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(conns) < 3 {
		t.Fatalf("the copy took %d chunks, want a window of at least three", len(conns))
	}
	for i, conn := range conns {
		if conn == nil || conn != conns[0] {
			t.Fatalf("chunk %d arrived on another connection than the first", i)
		}
	}
}

// TestAnExportCopyIsVerifiedAgainstTheDigestItsRenderAnnounced: a first
// chunk whose digest matches bytes other than the ones rendered does not
// publish them.
func TestAnExportCopyIsVerifiedAgainstTheDigestItsRenderAnnounced(t *testing.T) {
	t.Parallel()
	pair, faults, threadID := newExportPair(t, 1024)
	faults.exportChunk = func(_ context.Context, _ int64, chunk *RemoteArtifactChunk) {
		chunk.Data[len(chunk.Data)-1] ^= 1
		sum := sha256.Sum256(chunk.Data)
		chunk.SHA256 = hex.EncodeToString(sum[:])
	}
	_, err := exportToFile(pair, t, threadID)
	if code, _, _ := errorsx.PublicDetails(err); code != "remote_artifact_changed" {
		t.Fatalf("a copy unlike its render = %v, want remote_artifact_changed", err)
	}
	if exports := countReachExports(t, filepath.Join(pair.source.configDir, threadExportDirName), ""); exports != 0 {
		t.Fatalf("%d file(s) published from a copy unlike its render", exports)
	}
}

// TestACopyOfAnotherSizeThanAnnouncedIsRefused: the size is checked against
// the announcement at the first chunk, before any byte is written.
func TestACopyOfAnotherSizeThanAnnouncedIsRefused(t *testing.T) {
	t.Parallel()
	data := []byte("the rendered window")
	sum := sha256.Sum256(data)
	announced := artifactAnnouncement{Size: int64(len(data)) + 1, SHA256: hex.EncodeToString(sum[:])}
	_, err := receiveRemoteArtifact(t.Context(), t.TempDir(), "export", "thread", announced,
		func(context.Context, RemoteArtifactRequest) (RemoteArtifactChunk, error) {
			return RemoteArtifactChunk{Data: data, Name: "export", Size: int64(len(data)), Stamp: "stamp", SHA256: announced.SHA256, SourceThreadID: "thread"}, nil
		})
	if code, _, _ := errorsx.PublicDetails(err); code != "remote_artifact_changed" {
		t.Fatalf("a copy of another size than announced = %v, want remote_artifact_changed", err)
	}
}

// TestAWorkspaceArtifactsFirstReadIsDigestedWhateverItsStamp: a stamp says
// nothing of whether its reader holds the file's digest, so a command
// artifact's first read carries the digest of what is on disk now, stamped
// or not.
func TestAWorkspaceArtifactsFirstReadIsDigestedWhateverItsStamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report"), []byte("written"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("written"))
	want := hex.EncodeToString(sum[:])
	first, err := readRemoteArtifactChunk(t.Context(), dir, RemoteArtifactRequest{Path: "report"}, "")
	if err != nil || first.SHA256 != want {
		t.Fatalf("an unstamped first read = %+v, %v; want digest %s", first, err, want)
	}
	stamped, err := readRemoteArtifactChunk(t.Context(), dir, RemoteArtifactRequest{Path: "report", Stamp: first.Stamp}, "")
	if err != nil || stamped.SHA256 != want || string(stamped.Data) != "written" {
		t.Fatalf("a stamped first read = %+v, %v; want its bytes and digest %s", stamped, err, want)
	}
	// A recorded digest stands for the file only under the stamp it was
	// recorded with.
	unpinned, err := readRemoteArtifactChunk(t.Context(), dir, RemoteArtifactRequest{Path: "report"}, "recorded")
	if err != nil || unpinned.SHA256 != want {
		t.Fatalf("a first read with a recorded digest and no stamp = %+v, %v; want digest %s", unpinned, err, want)
	}
}

// TestAPeerExportIsServedUnderWhatItsRenderRecorded: the first chunk
// carries the digest the render took, for a receiver from v0.0.19 that
// verifies against it, without reading the file a second time; a file
// that changed since its render is refused; and the record goes with the
// file a day later.
func TestAPeerExportIsServedUnderWhatItsRenderRecorded(t *testing.T) {
	t.Parallel()
	pair, _, threadID := newExportPair(t, 1024)
	result, err := exportToFile(pair, t, threadID)
	if err != nil {
		t.Fatal(err)
	}
	file, _ := result["file"].(map[string]any)
	announced, _ := file["sha256"].(string)
	exportID := onlyPeerExport(t, pair.dest)
	path := filepath.Join(pair.dest.configDir, threadExportDirName, exportID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The same bytes in a different order under the same stamp: a second
	// read would digest them differently.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	swapped := append([]byte{data[len(data)-1]}, data[:len(data)-1]...)
	if err := os.WriteFile(path, swapped, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	chunk, err := pair.dest.readThreadExportChunk(ctx, exportID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.SHA256 != announced {
		t.Fatalf("the first chunk carries digest %s, want the render's %s", chunk.SHA256, announced)
	}

	// A file whose stamp moved is refused at every offset.
	if err := os.Chtimes(path, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{0, 1} {
		if _, err := pair.dest.readThreadExportChunk(ctx, exportID, offset); err == nil {
			t.Fatalf("a changed export was served at offset %d", offset)
		}
	}

	pair.dest.sweepThreadExports(time.Now().Add(2 * threadRequestExportAge))
	_, err = pair.dest.readThreadExportChunk(ctx, exportID, 0)
	if _, message, _ := errorsx.PublicDetails(err); !strings.Contains(message, "no longer on this computer") {
		t.Fatalf("a swept export = %v, want it named gone", err)
	}
	if _, kept := pair.dest.peerExports.get(exportID); kept {
		t.Fatal("the sweep kept the record of an export it removed")
	}
}

// TestAPairingThatEndsMidCopyEndsTheCopy: forgetting the computer, or
// pairing with it again, while a copy holds a connection to it closes that
// connection too. No further chunk is asked for under the old pairing, the
// copy fails with the pairing's verdict, and nothing it staged is left.
func TestAPairingThatEndsMidCopyEndsTheCopy(t *testing.T) {
	t.Parallel()
	for _, ending := range []struct {
		name string
		// apply runs on the destination's handler goroutine, so it takes
		// the subtest's StepBudget and answers its error, never t.Fatal.
		apply func(t *testing.T, pair *reachPair, budget func() (time.Duration, error)) error
		code  string
	}{
		{"forgotten", func(_ *testing.T, pair *reachPair, _ func() (time.Duration, error)) error {
			return pair.source.RemoveBackend(pair.computer, true)
		}, "remote_not_paired"},
		{"paired again", func(t *testing.T, pair *reachPair, budget func() (time.Duration, error)) error {
			wait, err := budget()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			invite, _ := pair.destWire.mintLink(t, "full")
			_, err = pair.manager.Add(ctx, invite.URL)
			return err
		}, "remote_pairing_replaced"},
	} {
		t.Run(ending.name, func(t *testing.T) {
			t.Parallel()
			pair, faults, threadID := newExportPair(t, 3*remoteArtifactChunkBytes)
			budget := testutil.StepBudget(t)
			var served atomic.Int32
			faults.exportChunk = func(context.Context, int64, *RemoteArtifactChunk) {
				if served.Add(1) == 1 {
					if err := ending.apply(t, pair, budget); err != nil {
						t.Errorf("end the pairing mid-copy: %v", err)
					}
				}
			}
			_, err := exportToFile(pair, t, threadID)
			if code, _, _ := errorsx.PublicDetails(err); code != ending.code {
				t.Fatalf("a copy whose pairing was %s mid-copy = %v, want %s", ending.name, err, ending.code)
			}
			if got := served.Load(); got != 1 {
				t.Fatalf("the destination served %d chunks, want only the one asked for before the pairing was %s", got, ending.name)
			}
			entries, err := os.ReadDir(filepath.Join(pair.source.configDir, threadExportDirName))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("the ended copy left %d entries in the export directory", len(entries))
			}
		})
	}
}

// askedOffsets is every offset the destination was asked for, in order.
func (p *peerFaults) askedOffsets() []int64 {
	p.exportMu.Lock()
	defer p.exportMu.Unlock()
	return slices.Clone(p.exportAsked)
}

// TestAnExportChunkWhoseConnectionWasLostIsReadAgain: the connection a copy
// holds closes under a chunk while the pairing stands, as a network change
// closes it. That chunk is asked for once more, at the same offset, over a
// new connection, and the copy completes.
func TestAnExportChunkWhoseConnectionWasLostIsReadAgain(t *testing.T) {
	t.Parallel()
	pair, faults, threadID := newExportPair(t, 2*remoteArtifactChunkBytes)
	var dropped atomic.Bool
	faults.exportChunk = func(ctx context.Context, offset int64, _ *RemoteArtifactChunk) {
		if offset == remoteArtifactChunkBytes && dropped.CompareAndSwap(false, true) {
			// The connection goes, the session it rode on stays live.
			pair.destWire.srv.SessionConns().CloseSession(transport.ConnStateFromContext(ctx).SessionID())
		}
	}
	result, err := exportToFile(pair, t, threadID)
	if err != nil {
		t.Fatalf("a copy that lost its connection once = %v, want it completed", err)
	}
	file, _ := result["file"].(map[string]any)
	path, _ := file["path"].(string)
	if info, err := os.Stat(path); err != nil || info.Size() <= 2*remoteArtifactChunkBytes {
		t.Fatalf("the published copy = %v, %v; want the whole export", info, err)
	}
	asked := faults.askedOffsets()
	want := []int64{0, remoteArtifactChunkBytes, remoteArtifactChunkBytes, 2 * remoteArtifactChunkBytes}
	if !slices.Equal(asked, want) {
		t.Fatalf("offsets asked = %v, want %v: the lost chunk once more, then on", asked, want)
	}
}

// TestAnExportChunkTheDestinationRefusedIsNotReadAgain: a refusal is the
// destination's answer, not a lost connection, so the copy ends on it.
func TestAnExportChunkTheDestinationRefusedIsNotReadAgain(t *testing.T) {
	t.Parallel()
	pair, faults, threadID := newExportPair(t, 2*remoteArtifactChunkBytes)
	faults.exportChunk = func(_ context.Context, offset int64, _ *RemoteArtifactChunk) {
		if offset != 0 {
			return
		}
		// The export's file changes under the copy, so the next chunk is
		// refused under the stamp its render recorded.
		path := filepath.Join(pair.dest.configDir, threadExportDirName, onlyPeerExport(t, pair.dest))
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(path, later, later); err != nil {
			t.Error(err)
		}
	}
	_, err := exportToFile(pair, t, threadID)
	if code, _, _ := errorsx.PublicDetails(err); code != "remote_artifact_changed" {
		t.Fatalf("a copy whose chunk was refused = %v, want remote_artifact_changed", err)
	}
	if asked, want := faults.askedOffsets(), []int64{0, remoteArtifactChunkBytes}; !slices.Equal(asked, want) {
		t.Fatalf("offsets asked = %v, want %v: a refused chunk is not asked for again", asked, want)
	}
}
