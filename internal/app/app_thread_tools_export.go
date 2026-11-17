package app

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/threadtools"
)

// `to_file` across computers.
//
// A path is only useful on the computer that can open it, so an export a
// paired computer asked for is rendered here, named by an export id, and
// copied over the pairing in the same 256 KiB pieces a command artifact
// uses. The model on the other computer reads a path in its OWN export
// directory and never learns this one's.

// threadPeerExportPrefix marks an export rendered for another computer.
// The sweep uses it to tell a file awaiting transfer from a person's own
// artefact, which is kept until they remove it.
const threadPeerExportPrefix = "peer-"

// threadPeerExportName is the export id AND the file name: the thread the
// window came from, plus a nonce so two transfers of the same thread never
// share a file. Both halves are UUIDs, so the id parses back without a
// table and a name from anywhere else fails to parse.
func threadPeerExportName(threadID string) string {
	return threadPeerExportPrefix + threadID + "." + entityid.New() + ".txt"
}

// threadPeerExportThread reads the thread id back out of an export id, and
// reports false for anything this computer did not mint.
func threadPeerExportThread(name string) (string, bool) {
	if !strings.HasPrefix(name, threadPeerExportPrefix) || !strings.HasSuffix(name, ".txt") {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, threadPeerExportPrefix), ".txt")
	threadID, nonce, cut := strings.Cut(rest, ".")
	if !cut || !entityid.Valid(threadID) || !entityid.Valid(nonce) {
		return "", false
	}
	return threadID, true
}

// threadExportTooLarge refuses a rendered export no transfer can carry. It
// names the size and what to narrow, because the agent chose the window
// and is the only one that can make it smaller.
func threadExportTooLarge(size int64) error {
	return errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("That window renders to %d bytes on the other computer, above the %d byte transfer limit. Narrow the window (fewer turns, or a since bound) or drop entries from include.",
			size, remoteArtifactMaxBytes), nil)
}

// peerExport is what the render of an export for another computer
// recorded: its digest and its file's stamp.
type peerExport struct {
	sha256, stamp string
	renderedAt    time.Time
}

// peerExports holds this computer's peer exports' records for as long as
// their files are kept (sweepThreadExports). The zero value is ready.
type peerExports struct {
	mu      sync.Mutex
	records map[string]peerExport
}

func (p *peerExports) put(exportID string, record peerExport) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records == nil {
		p.records = map[string]peerExport{}
	}
	p.records[exportID] = record
}

func (p *peerExports) get(exportID string) (peerExport, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, ok := p.records[exportID]
	return record, ok
}

// forgetBefore drops the records of exports rendered before cutoff.
func (p *peerExports) forgetBefore(cutoff time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	maps.DeleteFunc(p.records, func(_ string, record peerExport) bool { return record.renderedAt.Before(cutoff) })
}

// fetchThreadExport copies an export the destination rendered into this
// computer's own export directory and reports the local path.
//
// Every chunk goes over one held connection, so the copy pays one ping or
// dial rather than one per chunk. The copy is verified against the size and
// digest the render announced before the file is published, so a partial
// or changed transfer leaves nothing for the model to read.
func (a *App) fetchThreadExport(ctx context.Context, computer threadtools.Computer, file threadtools.ExportFile) (threadtools.ExportFile, error) {
	threadID, ok := threadPeerExportThread(file.ExportID)
	if !ok {
		return threadtools.ExportFile{}, errorsx.Public(threadtools.CodeUnreachable,
			fmt.Sprintf("%s answered to_file without an export this computer can fetch.", threadtools.NameOfComputer(computer)), nil)
	}
	if a.configDir == "" {
		return threadtools.ExportFile{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"This computer has no data directory configured, so a transcript cannot be written to a file.", nil)
	}
	if file.Size > remoteArtifactMaxBytes {
		return threadtools.ExportFile{}, threadExportTooLarge(file.Size)
	}
	dir := filepath.Join(a.configDir, threadExportDirName)
	var artifact RemoteArtifact
	err := a.backends.HoldThreadPeer(ctx, computer.ID, func(call attachedbackends.PeerCall) error {
		var err error
		artifact, err = receiveRemoteArtifact(ctx, dir, file.ExportID, threadID, artifactAnnouncement{Size: file.Size, SHA256: file.SHA256},
			func(ctx context.Context, request RemoteArtifactRequest) (RemoteArtifactChunk, error) {
				callCtx, cancel := context.WithTimeout(ctx, threadPeerCallWait("ThreadToolExportChunk"))
				defer cancel()
				var chunk RemoteArtifactChunk
				err := call(callCtx, "ThreadToolExportChunk", &chunk, file.ExportID, request.Offset)
				return chunk, err
			})
		return err
	})
	if err != nil {
		return threadtools.ExportFile{}, a.threadOperationError("export", computer.ID, threadID, err)
	}
	return threadtools.ExportFile{Path: artifact.Path, Size: artifact.Size, SHA256: artifact.SHA256}, nil
}

// readThreadExportChunk serves one piece of an export this computer
// rendered for a paired computer.
//
// Every chunk is read under the stamp the render recorded, because the
// peer method takes an offset and nothing else, so a file that changed
// since its render is refused at any offset. The first chunk carries the
// recorded digest rather than a second read of the whole file: a receiver
// from v0.0.19 verifies against it, a later one against the digest the
// render announced, which is the same.
func (a *App) readThreadExportChunk(ctx context.Context, exportID string, offset int64) (RemoteArtifactChunk, error) {
	threadID, ok := threadPeerExportThread(exportID)
	if !ok {
		return RemoteArtifactChunk{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"That is not an export this computer rendered for another computer.", nil)
	}
	if a.configDir == "" {
		return RemoteArtifactChunk{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"This computer has no data directory configured, so it holds no exports.", nil)
	}
	record, ok := a.peerExports.get(exportID)
	if !ok {
		return RemoteArtifactChunk{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"That export is no longer on this computer. Ask for the window again.", nil)
	}
	dir := filepath.Join(a.configDir, threadExportDirName)
	chunk, err := readRemoteArtifactChunk(ctx, dir, RemoteArtifactRequest{Path: exportID, Offset: offset, Stamp: record.stamp})
	if err != nil {
		return RemoteArtifactChunk{}, err
	}
	if offset == 0 {
		chunk.SHA256 = record.sha256
	}
	chunk.SourceThreadID = threadID
	return chunk, nil
}

// recordPeerExport records a published peer export's digest and stamp for
// the chunks that serve it.
func (a *App) recordPeerExport(exportID, path, digest string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("thread tools: read the published export: %w", err)
	}
	a.peerExports.put(exportID, peerExport{sha256: digest, stamp: remoteArtifactStamp(info), renderedAt: time.Now()})
	return nil
}
