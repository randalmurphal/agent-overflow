package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/atomicfile"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provider/codex/rollout"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtransfer"
	"agent-overflow/internal/transferfiles"
	"agent-overflow/internal/transferwire"
)

const transferManifestVersion = 1
const transferManifestLimit = 16 << 20

type transferManifest struct {
	Version     int                       `json:"version"`
	Intent      ThreadTransferIntent      `json:"intent"`
	Thread      store.Thread              `json:"thread"`
	SessionRef  string                    `json:"sessionRef"`
	Native      []store.TransferSession   `json:"native"`
	NativePaths map[string]string         `json:"nativePaths"`
	Attachments []store.Attachment        `json:"attachments"`
	Workspace   *gitops.TransferWorkspace `json:"workspace,omitempty"`
}

func (a *App) snapshotThreadTransfer(ctx context.Context, row store.ThreadTransfer, directory string) (transferwire.Upload, error) {
	var completed transferwire.Upload
	if found, err := atomicfile.ReadJSON(filepath.Join(directory, "snapshot-complete.json"), &completed); err != nil {
		return completed, err
	} else if found {
		if !completed.Valid() {
			return completed, errors.New("The saved transfer snapshot is invalid.")
		}
		return completed, nil
	}
	unlock, err := a.threadLocks().LockCtx(ctx, row.ThreadID)
	if err != nil {
		return completed, err
	}
	defer unlock()
	thread, err := a.store.GetThread(row.ThreadID)
	if err != nil {
		return completed, err
	}
	if err := a.checkTransferIdle(thread); err != nil {
		return completed, err
	}
	if err := a.stopExistingSessionLocked(row.ThreadID); err != nil {
		return completed, err
	}
	if a.triage != nil {
		if err := a.triage.FlushThread(row.ThreadID); err != nil {
			return completed, err
		}
	}
	// Closing drains final provider messages. Read again after the writer has
	// relinquished ownership, and refuse any newly observed unfinished work.
	thread, err = a.store.GetThread(row.ThreadID)
	if err != nil {
		return completed, err
	}
	if err := a.checkTransferIdle(thread); err != nil {
		return completed, err
	}
	var private threadtransfer.SourceData
	var details transferSourceDetails
	if json.Unmarshal(row.PrivateState, &private) != nil || json.Unmarshal(private.Details, &details) != nil || thread.Provider != details.Provider {
		return completed, errors.New("The transfer source configuration changed.")
	}
	if details.IncludeWorkspace {
		if err := a.ensureWorkspaceChangeAllowed("copy workspace changes", thread.WorkspacePath); err != nil {
			return completed, err
		}
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return completed, err
	}
	scratch := filepath.Join(directory, "snapshot")
	// There is no sealed receipt yet. These fixed, private paths can contain
	// only our failed preparation; a successful marker is checked above.
	if err := os.RemoveAll(scratch); err != nil {
		return completed, err
	}
	if err := os.Remove(filepath.Join(directory, "archive.tar")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return completed, err
	}
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return completed, err
	}
	manifest := transferManifest{Version: transferManifestVersion, Intent: a.transferIntent(row, details), Thread: thread, SessionRef: thread.ResolvedSessionRef(), NativePaths: make(map[string]string)}
	native, refs, err := a.collectTransferNative(ctx, thread, manifest.NativePaths)
	if err != nil {
		return completed, err
	}
	nativeUnlock, err := a.store.LockNativeSessions(ctx, refs)
	if err != nil {
		return completed, err
	}
	defer nativeUnlock()
	if thread.PendingForkRef == "" {
		if err := a.store.CheckTransferSourceAliases(thread.ID, refs); err != nil {
			return completed, err
		}
	}
	stamps, err := snapshotSourceStamps(native)
	if err != nil {
		return completed, err
	}
	if thread.Provider == "codex" && len(native) > 0 {
		native, err = rollout.FlattenTransferFiles(ctx, filepath.Join(scratch, "materialized"), native, manifest.NativePaths)
		if err != nil {
			return completed, err
		}
		// A materialized compressed leaf is plain JSONL. The logical archive
		// name otherwise stays stable across this provider-owned conversion.
		for id, name := range manifest.NativePaths {
			for _, file := range native {
				if file.Name == strings.TrimSuffix(name, ".zst") {
					manifest.NativePaths[id] = file.Name
					break
				}
			}
		}
	}
	var remap map[string]string
	// A lazy fork borrows its parent's transcript. Even Move must materialize
	// an independent native identity; only this fork's AO ownership moves.
	if (row.Kind == "copy" || thread.PendingForkRef != "") && len(native) > 0 {
		copyDir := filepath.Join(scratch, "copy")
		original := native
		switch thread.Provider {
		case "claude":
			cursor := ""
			if thread.PendingForkResumeAt != "" {
				projects, err := a.claudeProjectsDir()
				if err != nil {
					return completed, err
				}
				cut, err := claude.ResolveForkResumeCursor(projects, manifest.SessionRef, thread.WorkspacePath, thread.PendingForkResumeAt)
				if err != nil {
					return completed, err
				}
				if !cut.PinOnDisk || cut.Cursor == "" {
					return completed, errors.New("The fork's saved position is not available in its provider transcript yet. Retry after the original turn has saved.")
				}
				cursor = cut.Cursor
			}
			copied, err := sessionfork.CopyTransferFilesAt(ctx, sessionfork.TransferCopyCut{OperationID: row.ID, SessionID: manifest.SessionRef, Destination: copyDir, ThroughUUID: cursor}, native)
			if err != nil {
				return completed, err
			}
			manifest.SessionRef, native = copied.SessionID, copied.Files
			refs = []store.TransferSession{{Provider: "claude", Ref: copied.SessionID}}
		case "codex":
			copied, err := rollout.CopyTransferFiles(ctx, row.ID, copyDir, native)
			if err != nil {
				return completed, err
			}
			manifest.SessionRef, native, remap = copied.IDs[thread.SessionRef], copied.Files, copied.IDs
			for i := range refs {
				refs[i].Ref = copied.IDs[refs[i].Ref]
			}
		}
		paths := make(map[string]string, len(manifest.NativePaths))
		for id, name := range manifest.NativePaths {
			newID := manifest.SessionRef
			if thread.Provider == "codex" {
				newID = remap[id]
			}
			for i, file := range original {
				if file.Name == name {
					paths[newID] = native[i].Name
					break
				}
			}
		}
		manifest.NativePaths = paths
	}
	manifest.Native = refs
	if len(refs) > 0 {
		if err := a.store.BindThreadTransferSessions(row.ID, refs); err != nil {
			return completed, err
		}
	}
	sources := append([]transferfiles.Source(nil), native...)
	if details.IncludeWorkspace {
		capture, err := a.git.CaptureTransferWorkspace(ctx, thread.WorkspacePath)
		if err != nil {
			return completed, err
		}
		manifest.Workspace = &capture.Workspace
		if err := writeTransferFile(filepath.Join(scratch, "objects.pack"), func(w io.Writer) error {
			return a.git.WriteTransferObjects(ctx, thread.WorkspacePath, capture.Workspace.Head, capture.Workspace.Index, nil, w)
		}); err != nil {
			return completed, err
		}
		sources = append(sources, transferfiles.Source{Root: scratch, Path: "objects.pack", Name: "workspace/objects.pack"})
		sources = append(sources, capture.Sources...)
		// Verify after every source file has streamed, below.
		return a.finishTransferSnapshot(ctx, row, directory, scratch, manifest, sources, remap, stamps, func() error { return a.git.VerifyTransferWorkspace(ctx, thread.WorkspacePath, capture) })
	}
	return a.finishTransferSnapshot(ctx, row, directory, scratch, manifest, sources, remap, stamps, nil)
}

func (a *App) finishTransferSnapshot(ctx context.Context, row store.ThreadTransfer, directory, scratch string, manifest transferManifest, sources []transferfiles.Source, remap map[string]string, stamps []transferSourceStamp, verifyWorkspace func() error) (transferwire.Upload, error) {
	var receipt transferwire.Upload
	attachments, err := a.store.ThreadTransferAttachments(ctx, row.ThreadID)
	if err != nil {
		return receipt, err
	}
	manifest.Attachments = attachments
	for _, file := range attachments {
		sources = append(sources, transferfiles.Source{Root: a.attachments.Root(), Path: file.RelativePath, Name: "attachments/" + file.RelativePath})
	}
	options := store.ThreadHistoryExport{}
	if len(remap) > 0 {
		options.ItemMeta = func(meta string) (string, error) {
			return itemmeta.TransferCodexSessions(meta, remap)
		}
	}
	if err := writeTransferFile(filepath.Join(scratch, "history.ndjson"), func(w io.Writer) error { return a.store.ExportThreadHistoryWith(ctx, row.ThreadID, w, options) }); err != nil {
		return receipt, err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return receipt, err
	}
	if len(encoded) > transferManifestLimit {
		return receipt, errors.New("Conversation transfer metadata exceeds the supported limit.")
	}
	if err := atomicfile.Write(filepath.Join(scratch, "manifest.json"), encoded); err != nil {
		return receipt, err
	}
	sources = append(sources, transferfiles.Source{Root: scratch, Path: "history.ndjson", Name: "history.ndjson"}, transferfiles.Source{Root: scratch, Path: "manifest.json", Name: "manifest.json"})
	archive := filepath.Join(directory, "archive.tar")
	digest, err := transferfiles.Create(ctx, archive, sources)
	if err != nil {
		return receipt, err
	}
	if err := verifySourceStamps(stamps); err != nil {
		return receipt, err
	}
	if verifyWorkspace != nil {
		if err := verifyWorkspace(); err != nil {
			return receipt, err
		}
	}
	info, err := os.Stat(archive)
	if err != nil {
		return receipt, err
	}
	receipt = transferwire.Upload{SHA256: digest, Size: info.Size()}
	return receipt, atomicfile.WriteJSON(filepath.Join(directory, "snapshot-complete.json"), receipt)
}

func (a *App) collectTransferNative(ctx context.Context, thread store.Thread, paths map[string]string) ([]transferfiles.Source, []store.TransferSession, error) {
	if thread.PendingForkRef != "" && thread.Provider != "claude" {
		return nil, nil, errors.New("This provider's pending fork format cannot be transferred.")
	}
	if thread.ResolvedSessionRef() == "" {
		return nil, nil, nil
	}
	switch thread.Provider {
	case "claude":
		home, err := a.claudeProjectsDir()
		if err != nil {
			return nil, nil, err
		}
		ref := thread.ResolvedSessionRef()
		files, err := sessionfork.TransferFiles(ctx, home, ref, thread.WorkspacePath)
		if err == nil {
			paths[ref] = files[0].Name
		}
		return files, []store.TransferSession{{Provider: "claude", Ref: ref}}, err
	case "codex":
		home, err := a.providerHome()
		if err != nil {
			return nil, nil, err
		}
		cfg := a.codexProbeConfig(a.providerBinaryPath("codex"), nil)
		snapshot, err := codex.ReadTransferSnapshot(ctx, codex.TransferSnapshotConfig{Binary: cfg.Binary, Home: filepath.Join(home, ".codex"), WorkDir: cfg.WorkDir, Env: cfg.Env}, thread.SessionRef)
		if err != nil {
			return nil, nil, err
		}
		refs := make([]store.TransferSession, 0, len(snapshot.References))
		for _, ref := range snapshot.References {
			refs = append(refs, store.TransferSession{Provider: "codex", Ref: ref.SessionID})
			for _, file := range snapshot.Files {
				if strings.TrimSuffix(filepath.Clean(ref.Path), ".zst") == strings.TrimSuffix(filepath.Join(file.Root, filepath.FromSlash(file.Path)), ".zst") {
					paths[ref.SessionID] = file.Name
					break
				}
			}
			if paths[ref.SessionID] == "" {
				return nil, nil, errors.New("The current Codex rollout is missing from the snapshot.")
			}
		}
		return snapshot.Files, refs, nil
	}
	return nil, nil, errors.New("This provider cannot transfer native conversations.")
}

type transferSourceStamp struct {
	source transferfiles.Source
	info   os.FileInfo
}

func snapshotSourceStamps(sources []transferfiles.Source) ([]transferSourceStamp, error) {
	result := make([]transferSourceStamp, 0, len(sources))
	for _, source := range sources {
		root, err := os.OpenRoot(source.Root)
		if err != nil {
			return nil, err
		}
		info, err := root.Lstat(filepath.FromSlash(source.Path))
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("A native conversation file was replaced.")
		}
		result = append(result, transferSourceStamp{source, info})
	}
	return result, nil
}
func verifySourceStamps(stamps []transferSourceStamp) error {
	for _, before := range stamps {
		now, err := snapshotSourceStamps([]transferfiles.Source{before.source})
		if err != nil {
			return err
		}
		a, b := before.info, now[0].info
		if !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) || a.Mode() != b.Mode() {
			return errors.New("The provider's saved conversation changed while it was being copied. Retry after the other writer has stopped.")
		}
	}
	return nil
}
func writeTransferFile(filename string, write func(io.Writer) error) (err error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err = write(file); err != nil {
		return err
	}
	return file.Sync()
}

// Only explicit workflow-mode threads need their owning workflow transferred;
// they must not become detached runners as an incidental conversation move.
func transferManagedMode(mode string) bool { return strings.HasPrefix(mode, "workflow") }
