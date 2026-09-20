package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// fetchThreadExport copies an export the destination rendered into this
// computer's own export directory and reports the local path.
//
// The digest is verified over the whole copy before the file is published,
// so a partial or changed transfer leaves nothing for the model to read.
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
	artifact, err := receiveRemoteArtifact(ctx, dir, file.ExportID, threadID,
		func(ctx context.Context, request RemoteArtifactRequest) (RemoteArtifactChunk, error) {
			call, cancel := context.WithTimeout(ctx, threadPeerCallTimeout)
			defer cancel()
			var chunk RemoteArtifactChunk
			err := a.backends.CallThreadPeer(call, computer.ID, "ThreadToolExportChunk", &chunk, file.ExportID, request.Offset)
			return chunk, err
		})
	if err != nil {
		return threadtools.ExportFile{}, a.threadOperationError("export", computer.ID, threadID, err)
	}
	return threadtools.ExportFile{Path: artifact.Path, Size: artifact.Size, SHA256: artifact.SHA256}, nil
}

// readThreadExportChunk serves one piece of an export this computer
// rendered for a paired computer.
//
// The stamp is read here rather than carried in the request, because the
// peer method takes an offset and nothing else. The guarantee is unchanged:
// the receiver compares every chunk's stamp against the first one's and
// verifies the finished copy against the digest, so a file that changed
// mid-transfer is refused instead of delivered half old. An export is
// written once under a name no second render reuses, so the case is
// already impossible by construction.
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
	dir := filepath.Join(a.configDir, threadExportDirName)
	request := RemoteArtifactRequest{Path: exportID, Offset: offset}
	if offset > 0 {
		info, err := os.Stat(filepath.Join(dir, exportID))
		if err != nil {
			return RemoteArtifactChunk{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"That export is no longer on this computer. Ask for the window again.", err)
		}
		request.Stamp = remoteArtifactStamp(info)
	}
	chunk, err := readRemoteArtifactChunk(ctx, dir, request)
	if err != nil {
		return RemoteArtifactChunk{}, err
	}
	chunk.SourceThreadID = threadID
	return chunk, nil
}
