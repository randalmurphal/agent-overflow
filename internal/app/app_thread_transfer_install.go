package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/provider/codex/rollout"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtransfer"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

type appTransferInstaller struct{ app *App }

func (installer appTransferInstaller) Discard(ctx context.Context, row store.ThreadTransfer, directory string) error {
	var private threadtransfer.DestinationData
	var details transferDestinationDetails
	if json.Unmarshal(row.PrivateState, &private) != nil || json.Unmarshal(private.Details, &details) != nil {
		return errors.New("The transfer cleanup settings are unreadable.")
	}
	if !details.Intent.IncludeWorkspace {
		return nil
	}
	var manifest transferManifest
	if err := readTransferJSON(filepath.Join(directory, "extracted", "manifest.json"), transferManifestLimit, &manifest); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if manifest.Intent != details.Intent || manifest.Workspace == nil {
		return errors.New("The transfer cleanup does not match this conversation.")
	}
	project, err := installer.app.store.GetProject(details.ProjectID)
	if err != nil {
		return err
	}
	return installer.app.git.DiscardTransferPreparation(ctx, row.ID, project.Path, details.WorkspacePath, manifest.Workspace.Head, details.Branch)
}

type transferInstallPlan struct {
	Version   int                          `json:"version"`
	Thread    store.Thread                 `json:"thread"`
	Native    []store.TransferSession      `json:"native"`
	Roots     map[string]string            `json:"roots"`
	Files     []transferfiles.Installation `json:"files"`
	Workspace *gitops.TransferWorktree     `json:"workspace,omitempty"`
}

func (installer appTransferInstaller) Prepare(ctx context.Context, row store.ThreadTransfer, stage string, files []transferfiles.File) (json.RawMessage, error) {
	a := installer.app
	var private threadtransfer.DestinationData
	var details transferDestinationDetails
	if json.Unmarshal(row.PrivateState, &private) != nil || json.Unmarshal(private.Details, &details) != nil {
		return nil, errors.New("The destination transfer settings are unreadable.")
	}
	var manifest transferManifest
	if err := readTransferJSON(filepath.Join(stage, "manifest.json"), transferManifestLimit, &manifest); err != nil {
		return nil, err
	}
	if manifest.Version != transferManifestVersion {
		return nil, errors.New("Update this computer before receiving this conversation format.")
	}
	if manifest.Intent != details.Intent || manifest.Thread.ID != details.Intent.SourceThreadID || manifest.Thread.Provider != details.Intent.Provider || row.ThreadID != details.Intent.TargetThreadID ||
		(manifest.Workspace != nil) != details.Intent.IncludeWorkspace {
		return nil, errors.New("The uploaded conversation does not match the accepted transfer.")
	}
	if transferManagedMode(manifest.Thread.Mode) || manifest.Thread.DiscussionID != "" || manifest.Thread.ParentThreadID != "" {
		return nil, errors.New("This conversation still belongs to another workflow or discussion.")
	}
	project, err := a.store.GetProject(details.ProjectID)
	if err != nil {
		return nil, err
	}
	unlock, err := a.threadLocks().LockCtx(ctx, row.ThreadID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	nativeUnlock, err := a.store.LockNativeSessions(ctx, manifest.Native)
	if err != nil {
		return nil, err
	}
	defer nativeUnlock()
	minimumCodexVersion, err := validateTransferredNative(ctx, stage, manifest, files)
	if err != nil {
		return nil, err
	}
	if err := a.providerDiscoveryService().CheckTransferReadiness(ctx, manifest.Thread.Provider, minimumCodexVersion); err != nil {
		return nil, err
	}
	if len(manifest.Native) > 0 {
		if err := a.store.BindThreadTransferSessions(row.ID, manifest.Native); err != nil {
			return nil, err
		}
	}
	target := manifest.Thread
	target.ID, target.ProjectID, target.ProjectPath = row.ThreadID, project.ID, project.Path
	target.WorkspacePath = details.WorkspacePath
	target.WorktreePath = ""
	if !gitops.SameFilesystemPath(project.Path, target.WorkspacePath) {
		target.WorktreePath = target.WorkspacePath
	}
	target.Branch = details.Branch
	target.RuntimeMode = details.Intent.RuntimeMode
	target.SessionRef, target.PendingForkRef, target.PendingForkResumeAt = manifest.SessionRef, "", ""
	target.DiscussionID, target.ParentThreadID, target.GroupID, target.ForkedFromThreadID = "", "", "", ""
	target.WorktreeSetupState, target.ImportSource = "", ""
	target.PinnedAt, target.PinGroup = nil, nil
	target.HasIncompleteTurn = false
	if row.Kind == "copy" {
		target.Archived = false
	}
	plan := transferInstallPlan{Version: transferManifestVersion, Thread: target, Native: manifest.Native}
	if manifest.Workspace != nil {
		if err := os.MkdirAll(filepath.Dir(target.WorkspacePath), 0o700); err != nil {
			return nil, err
		}
	}
	// Prevalidate the complete logical history in an isolated database. Do not
	// hold a writer transaction on the live app during multi-gigabyte validation.
	if err := validateTransferredHistory(ctx, stage, target, manifest.Attachments, a.store); err != nil {
		return nil, err
	}
	roots, err := a.transferInstallRoots()
	if err != nil {
		return nil, err
	}
	plan.Roots = roots
	targets, err := transferInstallTargets(manifest, target, files, roots)
	if err != nil {
		return nil, err
	}
	returning, err := a.store.ReturningTransferSessions(row.ID)
	if err != nil {
		return nil, err
	}
	returned := make(map[string]bool, len(returning))
	for _, ref := range returning {
		returned[ref.Provider+"/"+ref.Ref] = true
	}
	for i, target := range targets {
		switch target.Root {
		case "claude":
			targets[i].ReplaceExisting = returned["claude/"+manifest.SessionRef]
		case "codex":
			id, err := rollout.TransferFileSession(ctx, transferfiles.Source{Root: stage, Path: target.File.Name, Name: target.File.Name})
			if err != nil {
				return nil, err
			}
			targets[i].ReplaceExisting = returned["codex/"+id]
		}
	}
	plan.Files, err = transferfiles.PrepareInstallation(ctx, roots, targets)
	if err != nil {
		return nil, err
	}
	if manifest.Workspace != nil {
		pack, err := os.Open(filepath.Join(stage, "workspace", "objects.pack"))
		if err != nil {
			return nil, err
		}
		workspace, err := a.git.PrepareTransferWorktree(ctx, gitops.TransferWorktreeRequest{OperationID: row.ID, Repository: project.Path, Path: target.WorkspacePath, Branch: details.Branch, Workspace: *manifest.Workspace, ArchiveRoot: stage}, pack)
		closeErr := pack.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		plan.Workspace = &workspace
	} else if err := a.validateTransferCheckout(ctx, project.Path, target.WorkspacePath); err != nil {
		return nil, err
	}
	return json.Marshal(plan)
}

func (installer appTransferInstaller) Install(ctx context.Context, row store.ThreadTransfer, stage string, recipe json.RawMessage, secret []byte) error {
	a := installer.app
	var plan transferInstallPlan
	if len(recipe) > transferManifestLimit || json.Unmarshal(recipe, &plan) != nil || plan.Version != transferManifestVersion || plan.Thread.ID != row.ThreadID {
		return errors.New("The saved transfer installation plan is invalid.")
	}
	unlock, err := a.threadLocks().LockCtx(ctx, row.ThreadID)
	if err != nil {
		return err
	}
	defer unlock()
	nativeUnlock, err := a.store.LockNativeSessions(ctx, plan.Native)
	if err != nil {
		return err
	}
	defer nativeUnlock()
	if _, err := a.store.CheckThreadTransferActivation(row.ID, row.ManifestHash, secret); err != nil {
		return err
	}
	// The selected project must still exist and be the same repository. The
	// recipe is private, but an unrelated project mutation can happen meanwhile.
	project, err := a.store.GetProject(plan.Thread.ProjectID)
	if err != nil {
		return err
	}
	if !gitops.SameFilesystemPath(project.Path, plan.Thread.ProjectPath) {
		return errors.New("The destination project moved while the conversation was transferring.")
	}
	roots, err := a.transferInstallRoots()
	if err != nil {
		return err
	}
	for name, previous := range plan.Roots {
		if !gitops.SameFilesystemPath(previous, roots[name]) {
			return errors.New("The destination data directories changed. Restore them to finish this transfer.")
		}
	}
	if plan.Workspace != nil {
		if err := a.git.PublishTransferWorktree(ctx, *plan.Workspace); err != nil {
			return err
		}
	} else if err := a.validateTransferCheckout(ctx, project.Path, plan.Thread.WorkspacePath); err != nil {
		return err
	}
	if err := transferfiles.InstallPreparedFiles(ctx, stage, roots, plan.Files); err != nil {
		return err
	}
	history, err := os.Open(filepath.Join(stage, "history.ndjson"))
	if err != nil {
		return err
	}
	defer history.Close()
	_, err = a.store.CommitIncomingThreadTransfer(ctx, row.ID, row.ManifestHash, secret, plan.Thread, history)
	if err == nil {
		a.broadcastThreadRowByID(row.ThreadID)
	}
	return err
}

func (a *App) transferInstallRoots() (map[string]string, error) {
	home, err := a.providerHome()
	if err != nil {
		return nil, err
	}
	roots := map[string]string{"claude": sessionfork.ProjectsDirForHome(home), "codex": filepath.Join(home, ".codex"), "attachments": a.attachments.Root()}
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, err
		}
	}
	return roots, nil
}

func transferInstallTargets(manifest transferManifest, target store.Thread, files []transferfiles.File, roots map[string]string) ([]transferfiles.InstallTarget, error) {
	type attachmentTarget struct {
		path string
		size int64
	}
	attachments := make(map[string]attachmentTarget, len(manifest.Attachments))
	for _, a := range manifest.Attachments {
		installed, err := store.TransferredAttachment(a, target.ID)
		if err != nil {
			return nil, err
		}
		key := "attachments/" + a.RelativePath
		if _, exists := attachments[key]; exists {
			return nil, errors.New("The transfer repeats an attachment file.")
		}
		attachments[key] = attachmentTarget{installed.RelativePath, a.Size}
	}
	var claudeSlug string
	if target.Provider == "claude" && manifest.SessionRef != "" {
		// The final worktree need not exist yet. WorkspaceProjectDir encodes
		// canonical existing ancestors and its future basename consistently.
		directory, err := sessionfork.PlannedWorkspaceProjectDir(roots["claude"], target.WorkspacePath)
		if err != nil {
			return nil, err
		}
		claudeSlug = filepath.Base(directory)
	}
	targets := make([]transferfiles.InstallTarget, 0, len(files))
	for _, file := range files {
		switch {
		case strings.HasPrefix(file.Name, "attachments/"):
			attachment, ok := attachments[file.Name]
			if !ok {
				return nil, errors.New("The transfer includes an attachment outside this conversation.")
			}
			if file.Size != attachment.size {
				return nil, errors.New("The transferred attachment size does not match its history.")
			}
			delete(attachments, file.Name)
			targets = append(targets, transferfiles.InstallTarget{File: file, Root: "attachments", Path: attachment.path})
		case strings.HasPrefix(file.Name, "native/claude/"):
			parts := strings.SplitN(strings.TrimPrefix(file.Name, "native/claude/"), "/", 2)
			if len(parts) != 2 || claudeSlug == "" {
				return nil, errors.New("Invalid Claude transfer path.")
			}
			targets = append(targets, transferfiles.InstallTarget{File: file, Root: "claude", Path: path.Join(claudeSlug, parts[1])})
		case strings.HasPrefix(file.Name, "native/codex/"):
			targets = append(targets, transferfiles.InstallTarget{File: file, Root: "codex", Path: strings.TrimPrefix(file.Name, "native/codex/")})
		}
	}
	if len(attachments) != 0 {
		return nil, errors.New("The transfer is missing attachment files.")
	}
	return targets, nil
}

func validateTransferredNative(ctx context.Context, stage string, manifest transferManifest, files []transferfiles.File) (string, error) {
	if manifest.SessionRef == "" {
		if len(manifest.Native) != 0 || len(manifest.NativePaths) != 0 {
			return "", errors.New("A new conversation cannot contain another native session.")
		}
	} else if len(manifest.Native) == 0 || len(manifest.NativePaths) != len(manifest.Native) {
		return "", errors.New("The native conversation graph is incomplete.")
	}
	seen := make(map[string]bool, len(manifest.Native))
	for _, ref := range manifest.Native {
		if _, err := uuid.Parse(ref.Ref); err != nil || ref.Provider != manifest.Thread.Provider || seen[ref.Ref] {
			return "", errors.New("The native conversation identity is invalid.")
		}
		seen[ref.Ref] = true
		name := manifest.NativePaths[ref.Ref]
		if !transferfiles.ValidName(name) || !strings.HasPrefix(name, "native/"+ref.Provider+"/") {
			return "", errors.New("A native session points outside its transfer directory.")
		}
	}
	var collected []transferfiles.Source
	if manifest.SessionRef != "" {
		if !seen[manifest.SessionRef] {
			return "", errors.New("The native root session is missing.")
		}
		switch manifest.Thread.Provider {
		case "claude":
			if len(seen) != 1 {
				return "", errors.New("Claude child sessions must be inside their parent session directory.")
			}
			var err error
			collected, err = sessionfork.TransferFiles(ctx, filepath.Join(stage, "native", "claude"), manifest.SessionRef, "")
			if err != nil {
				return "", err
			}
			if len(collected) == 0 || collected[0].Name != manifest.NativePaths[manifest.SessionRef] {
				return "", errors.New("The Claude transcript does not match the selected native root.")
			}
		case "codex":
			resolve := func(ctx context.Context, id string) (rollout.TransferReference, error) {
				name := manifest.NativePaths[id]
				if name == "" {
					return rollout.TransferReference{}, errors.New("A saved child session is missing from the transfer.")
				}
				return rollout.TransferReference{SessionID: id, Path: filepath.Join(stage, filepath.FromSlash(name))}, nil
			}
			root, err := resolve(ctx, manifest.SessionRef)
			if err != nil {
				return "", err
			}
			refs, source, err := rollout.TransferGraph(ctx, filepath.Join(stage, "native", "codex"), root, resolve)
			if err != nil {
				return "", err
			}
			if len(refs) != len(seen) {
				return "", errors.New("The native transfer includes an unrelated session.")
			}
			collected = source
		}
	}
	expected := make(map[string]bool, len(collected))
	for _, source := range collected {
		expected[source.Name] = true
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name, "native/") {
			if !expected[file.Name] {
				return "", fmt.Errorf("Unexpected native transfer file: %s", file.Name)
			}
			delete(expected, file.Name)
		}
	}
	if len(expected) != 0 {
		return "", errors.New("The transfer is missing native session files.")
	}
	if manifest.Thread.Provider == "codex" {
		return rollout.TransferMinimumVersion(ctx, collected)
	}
	return "", nil
}

func validateTransferredHistory(ctx context.Context, stage string, target store.Thread, attachments []store.Attachment, destination *store.Store) error {
	directory, err := os.MkdirTemp(filepath.Dir(stage), "validate-history-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	db, err := store.New(filepath.Join(directory, "history.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.CreateProject(store.Project{ID: target.ProjectID, Path: target.ProjectPath, Name: "Transfer validation"}); err != nil {
		return err
	}
	history, err := os.Open(filepath.Join(stage, "history.ndjson"))
	if err != nil {
		return err
	}
	defer history.Close()
	if err := db.ImportThreadHistory(ctx, target, history); err != nil {
		return err
	}
	actual, err := db.ThreadTransferAttachments(ctx, target.ID)
	if err != nil {
		return err
	}
	if len(actual) != len(attachments) {
		return errors.New("The attachment manifest does not match the transferred history.")
	}
	expected := make(map[string]store.Attachment, len(attachments))
	for _, a := range attachments {
		mapped, err := store.TransferredAttachment(a, target.ID)
		if err != nil {
			return err
		}
		expected[mapped.ID] = mapped
	}
	for _, a := range actual {
		if want, ok := expected[a.ID]; !ok || want != a {
			return errors.New("The attachment metadata changed during transfer.")
		}
	}
	return destination.CheckTransferHistoryConflicts(ctx, db, target.ID)
}

func readTransferJSON(filename string, limit int64, value any) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("The transfer metadata is too large.")
	}
	return json.Unmarshal(data, value)
}
