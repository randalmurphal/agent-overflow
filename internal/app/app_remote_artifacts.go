package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
)

const remoteArtifactMaxBytes int64 = 1 << 30
const remoteArtifactChunkBytes = 256 << 10

type RemoteArtifactRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Stamp  string `json:"stamp,omitempty"`
}
type RemoteArtifactChunk struct {
	Data           []byte `json:"data"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Stamp          string `json:"stamp"`
	SHA256         string `json:"sha256,omitempty"`
	SourceThreadID string `json:"sourceThreadId"`
}
type RemoteArtifact struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	ComputerID string `json:"computerId"`
	RequestID  string `json:"requestId"`
}

// RemoteCommandArtifact reads a single regular file confined to the original
// job workspace. The receipt's authenticated owner can retrieve results after
// disabling new jobs. No provider state or filesystem write is involved.
//
//ao:scope terminal:operate
//ao:route selected
func (a *App) RemoteCommandArtifact(ctx context.Context, id string, request RemoteArtifactRequest) (RemoteArtifactChunk, error) {
	job, err := a.RemoteCommandStatus(ctx, id)
	if err != nil {
		return RemoteArtifactChunk{}, err
	}
	result, err := readRemoteArtifactChunk(ctx, job.Workspace, request)
	result.SourceThreadID = job.SourceThreadID
	return result, err
}

func readRemoteArtifactChunk(ctx context.Context, workspace string, request RemoteArtifactRequest) (RemoteArtifactChunk, error) {
	fail := func(code, message string, err error) (RemoteArtifactChunk, error) {
		return RemoteArtifactChunk{}, errorsx.Public(code, message, err)
	}
	if err := ctx.Err(); err != nil {
		return fail("remote_artifact_canceled", "Artifact retrieval was canceled; no complete local copy was published.", err)
	}
	name := request.Path
	if filepath.IsAbs(name) {
		var err error
		name, err = filepath.Rel(workspace, name)
		if err != nil {
			return fail("remote_artifact_path", "Choose a file inside the command's original workspace.", err)
		}
	}
	if !filepath.IsLocal(name) || name == "." || strings.ContainsRune(name, 0) || len(name) > 32768 || request.Offset < 0 || request.Offset > remoteArtifactMaxBytes || (request.Offset > 0 && request.Stamp == "") {
		return fail("remote_artifact_path", "Choose a file inside the command's original workspace, with a nonnegative offset and the stamp from its first chunk.", nil)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return fail("remote_artifact_workspace", "The command's original workspace is unavailable. Restore or mount it on the destination before retrieving this artifact.", err)
	}
	defer root.Close()
	file, err := openRemoteArtifact(root, name)
	if err != nil {
		return fail("remote_artifact_unavailable", "The artifact could not be opened. Check that the file exists, is readable, and that symlinks stay inside the command's original workspace.", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fail("remote_artifact_unavailable", "The artifact could not be inspected on the destination. Retry when the file is stable and readable.", err)
	}
	if !info.Mode().IsRegular() {
		return fail("remote_artifact_type", "Only regular files can be retrieved; directories, devices, and pipes are not artifacts.", nil)
	}
	if info.Size() > remoteArtifactMaxBytes {
		return fail("remote_artifact_too_large", "The artifact exceeds the 1 GiB transfer limit. Select a smaller report or split the file explicitly on the destination.", nil)
	}
	stamp := remoteArtifactStamp(info)
	changed := func() (RemoteArtifactChunk, error) {
		return fail("remote_artifact_changed", "The artifact changed during retrieval. Wait for its writer to finish, then retrieve it again; no complete local copy was published.", nil)
	}
	if request.Offset > info.Size() || (request.Stamp != "" && request.Stamp != stamp) {
		return changed()
	}
	result := RemoteArtifactChunk{Name: filepath.Base(name), Size: info.Size(), Stamp: stamp}
	if request.Offset == 0 {
		hash := sha256.New()
		// Compute once, streamed. The receiver verifies the finished copy against
		// this digest, so a writer restoring timestamps cannot create a mixed file.
		if _, err = io.Copy(hash, &remoteArtifactReader{ctx: ctx, reader: io.NewSectionReader(file, 0, info.Size())}); err != nil {
			return fail("remote_artifact_read", "Reading the artifact failed. Retry when the destination and file are available.", err)
		}
		result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	}
	length := min(int64(remoteArtifactChunkBytes), info.Size()-request.Offset)
	result.Data = make([]byte, int(length))
	if _, err = file.ReadAt(result.Data, request.Offset); err != nil {
		return changed()
	}
	after, err := file.Stat()
	if err != nil || remoteArtifactStamp(after) != stamp {
		return changed()
	}
	return result, nil
}

func remoteArtifactStamp(info os.FileInfo) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%d:%d", info.Size(), info.ModTime().UnixNano(), info.Mode()))
	return hex.EncodeToString(sum[:])
}

type remoteArtifactReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *remoteArtifactReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// AgentRemoteFetchArtifact copies one destination artifact into this computer's
// private artifact directory. Only the final verified path reaches the agent.
//
//ao:scope terminal:operate
//ao:route selected
func (a *App) AgentRemoteFetchArtifact(ctx context.Context, computerID, id, path string) (RemoteArtifact, error) {
	job, err := a.AgentRemoteStatus(ctx, computerID, id)
	if err != nil {
		return RemoteArtifact{}, err
	}
	if a.configDir == "" {
		return RemoteArtifact{}, errorsx.Public("remote_artifact_storage", "Local artifact storage is not ready. Wait for Agent Overflow startup to finish.", nil)
	}
	result, err := receiveRemoteArtifact(ctx, filepath.Join(a.configDir, "remote-artifacts"), path, job.SourceThreadID, func(ctx context.Context, request RemoteArtifactRequest) (RemoteArtifactChunk, error) {
		call, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var chunk RemoteArtifactChunk
		err := a.backends.CallAgentPeer(call, computerID, "RemoteCommandArtifact", &chunk, id, request)
		return chunk, err
	})
	if err != nil {
		return RemoteArtifact{}, remoteOperationError("fetch artifact", computerID, id, err)
	}
	result.ComputerID, result.RequestID = computerID, id
	return result, nil
}

func receiveRemoteArtifact(ctx context.Context, directory, path, threadID string, read func(context.Context, RemoteArtifactRequest) (RemoteArtifactChunk, error)) (result RemoteArtifact, err error) {
	// Create a private, unique directory: final rename cannot replace a previous
	// download or user file, and cancellation removes every partial byte.
	if err = os.MkdirAll(directory, 0o700); err != nil {
		return result, err
	}
	staging, err := os.MkdirTemp(directory, "artifact-")
	if err != nil {
		return result, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(staging)
		}
	}()
	output, err := os.OpenFile(filepath.Join(staging, ".partial"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return result, err
	}
	defer output.Close()
	hash := sha256.New()
	request := RemoteArtifactRequest{Path: path}
	var expected RemoteArtifactChunk
	for {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		chunk, readErr := read(ctx, request)
		if readErr != nil {
			return result, readErr
		}
		if chunk.SourceThreadID != threadID {
			return result, errorsx.Public("remote_wrong_conversation", "This artifact belongs to another conversation. Retrieve it from the conversation that submitted its job.", nil)
		}
		if request.Offset == 0 {
			digest, decodeErr := hex.DecodeString(chunk.SHA256)
			if chunk.Size < 0 || chunk.Size > remoteArtifactMaxBytes || decodeErr != nil || len(digest) != sha256.Size || chunk.Stamp == "" || !filepath.IsLocal(chunk.Name) || filepath.Base(chunk.Name) != chunk.Name || chunk.Name == ".partial" {
				return result, errors.New("destination returned invalid artifact metadata")
			}
			expected = chunk
			expected.Data = nil
			request.Stamp = chunk.Stamp
		}
		remaining := expected.Size - request.Offset
		if chunk.Stamp != expected.Stamp || chunk.Size != expected.Size || chunk.Name != expected.Name || int64(len(chunk.Data)) != min(int64(remoteArtifactChunkBytes), remaining) {
			return result, errorsx.Public("remote_artifact_changed", "The destination returned an inconsistent artifact. Wait for its writer to finish and retrieve it again.", nil)
		}
		if _, err = output.Write(chunk.Data); err != nil {
			return result, err
		}
		_, _ = hash.Write(chunk.Data)
		request.Offset += int64(len(chunk.Data))
		if request.Offset == expected.Size {
			break
		}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != expected.SHA256 {
		return result, errorsx.Public("remote_artifact_changed", "The artifact changed during retrieval. No complete local copy was published. Wait for the writer to finish, then retrieve it again.", nil)
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = output.Sync(); err != nil {
		return result, err
	}
	if err = output.Close(); err != nil {
		return result, err
	}
	final := filepath.Join(staging, expected.Name)
	if err = os.Rename(output.Name(), final); err != nil {
		return result, err
	}
	complete = true
	return RemoteArtifact{Path: final, Name: expected.Name, Size: expected.Size, SHA256: digest}, nil
}
