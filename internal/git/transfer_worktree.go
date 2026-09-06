package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

const transferWorktreePrefix = ".ao-transfer-"
const transferWorktreeMarker = "ao-transfer.json"

type TransferWorktreeRequest struct {
	OperationID string
	Repository  string
	Path        string
	Branch      string
	Workspace   TransferWorkspace
	ArchiveRoot string
}

// TransferWorktree is a private, durable publication recipe. GitDir and the
// operation marker identify this exact registered checkout across a rename;
// merely finding a directory at Path does not make an activation retry safe.
type TransferWorktree struct {
	OperationID string `json:"operationId"`
	Repository  string `json:"repository"`
	Stage       string `json:"stage"`
	Path        string `json:"path"`
	GitDir      string `json:"gitDir"`
	Head        string `json:"head"`
	Branch      string `json:"branch"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// PrepareTransferWorktree reserves a locked worktree in the destination repo.
// Its HEAD and index keep imported objects reachable through ordinary git gc;
// a --shared clone's alternates file provides no such retention guarantee.
// The existing checkout/index/branches are untouched. An optional NEW branch
// is reserved by worktree add; an existing branch is never reset or reused.
// Stage is a sibling of Path so publication cannot cross filesystem volumes.
func (c *Core) PrepareTransferWorktree(ctx context.Context, request TransferWorktreeRequest, objects io.Reader) (plan TransferWorktree, err error) {
	if _, err := uuid.Parse(request.OperationID); err != nil {
		return plan, errors.New("transfer: invalid workspace operation")
	}
	if !filepath.IsAbs(request.Repository) || !filepath.IsAbs(request.Path) || !filepath.IsAbs(request.ArchiveRoot) || objects == nil {
		return plan, errors.New("transfer: invalid workspace preparation paths")
	}
	if err := validateTransferWorkspace(request.Workspace); err != nil {
		return plan, err
	}
	if request.Branch != "" {
		if err := ValidateBranchName(request.Branch); err != nil {
			return plan, err
		}
	}
	if _, err := os.Lstat(request.Path); !errors.Is(err, fs.ErrNotExist) {
		return plan, errors.New("transfer: destination workspace already exists or is unavailable")
	}
	plan = TransferWorktree{OperationID: request.OperationID, Repository: filepath.Clean(request.Repository), Path: filepath.Clean(request.Path), Head: request.Workspace.Head, Branch: request.Branch}
	plan.Stage = filepath.Join(filepath.Dir(plan.Path), transferWorktreePrefix+request.OperationID)
	if plan.Stage == plan.Path {
		return plan, errors.New("transfer: destination uses the reserved preparation name")
	}
	if err := c.discardTransferPreparation(ctx, plan); err != nil {
		return plan, err
	}
	if err = os.MkdirAll(filepath.Dir(plan.Stage), 0o700); err != nil {
		return plan, err
	}
	if err = os.Mkdir(plan.Stage, 0o700); err != nil {
		return plan, err
	}
	defer func() {
		if err == nil {
			return
		}
		// A failed prepare has no activation promise. Cleanup has its own
		// small deadline even if the host canceled the long preparation.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if cleanupErr := c.discardTransferPreparation(cleanup, plan); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("transfer: remove failed preparation: %w", cleanupErr))
		}
	}()
	if err = c.ImportTransferObjects(ctx, plan.Repository, objects); err != nil {
		return plan, err
	}
	args := []string{"-c", "core.hooksPath=" + os.DevNull, "worktree", "add", "--quiet", "--no-checkout", "--lock", "--reason", transferWorktreeReason(plan.OperationID)}
	if plan.Branch != "" {
		args = append(args, "-b", plan.Branch)
	} else {
		args = append(args, "--detach")
	}
	args = append(args, "--", plan.Stage, plan.Head)
	if _, _, err = c.executeSpec(commandSpec{binary: "git", cwd: plan.Repository, ctx: ctx, args: args}); err != nil {
		return plan, err
	}
	var found bool
	plan.GitDir, found, err = c.revParsePath(plan.Stage, "--absolute-git-dir")
	if err != nil {
		return plan, err
	}
	if !found {
		return plan, errors.New("transfer: prepared worktree has no Git directory")
	}
	if err = atomicfile.WriteJSON(filepath.Join(plan.GitDir, transferWorktreeMarker), plan); err != nil {
		return plan, err
	}
	root, err := os.OpenRoot(plan.Stage)
	if err != nil {
		return plan, err
	}
	defer root.Close()
	archive, err := os.OpenRoot(request.ArchiveRoot)
	if err != nil {
		return plan, err
	}
	defer archive.Close()
	if err = c.materializeTransferWorkspace(ctx, plan.Stage, root, archive, request.Workspace); err != nil {
		return plan, err
	}
	// Flush working files and the registered index/HEAD before acknowledging
	// prepared. Imported immutable packs are flushed by ImportTransferObjects.
	if plan.Fingerprint, err = c.transferWorktreeFingerprint(ctx, plan.Stage, true); err != nil {
		return plan, err
	}
	if err = syncTransferTree(ctx, plan.GitDir, 1024); err != nil {
		return plan, err
	}
	if err = atomicfile.SyncDir(filepath.Dir(plan.GitDir)); err != nil {
		return plan, err
	}
	err = atomicfile.SyncDir(filepath.Dir(plan.Stage))
	return plan, err
}

func transferWorktreeReason(id string) string { return "Agent Overflow transfer " + id }

// DiscardTransferPreparation is only for an unactivated or canceled operation.
// It refuses published workspaces and verifies operation ownership before
// removing a registered preparation. A caller must serialize it with install.
func (c *Core) DiscardTransferPreparation(ctx context.Context, operationID, repository, destination, head, branch string) error {
	plan := TransferWorktree{OperationID: operationID, Repository: filepath.Clean(repository), Path: filepath.Clean(destination), Head: head, Branch: branch}
	plan.Stage = filepath.Join(filepath.Dir(plan.Path), transferWorktreePrefix+operationID)
	if _, err := uuid.Parse(operationID); err != nil || !filepath.IsAbs(repository) || !filepath.IsAbs(destination) || !validTransferOID(head) || plan.Stage == plan.Path {
		return errors.New("transfer: invalid preparation cleanup")
	}
	if branch != "" {
		if err := ValidateBranchName(branch); err != nil {
			return err
		}
	}
	return c.discardTransferPreparation(ctx, plan)
}

func (c *Core) discardTransferPreparation(ctx context.Context, plan TransferWorktree) error {
	cleanupMarker := plan.Stage + ".cleanup.json"
	var cleanup TransferWorktree
	marked, err := atomicfile.ReadJSON(cleanupMarker, &cleanup)
	if err != nil {
		return err
	}
	if marked {
		wanted := plan
		wanted.GitDir = cleanup.GitDir
		if transferWorktreeCoordinates(cleanup) != transferWorktreeCoordinates(wanted) || validateTransferWorktreePlan(cleanup) != nil {
			return errors.New("transfer: preparation cleanup belongs to another operation")
		}
	}
	info, err := os.Lstat(plan.Stage)
	if errors.Is(err, fs.ErrNotExist) {
		if marked {
			return c.finishTransferPreparationCleanup(ctx, cleanup, cleanupMarker)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("transfer: preparation path was replaced")
	}
	// A crash between mkdir and worktree add can leave an empty directory.
	// Remove alone is atomic and refuses anything with content, including a
	// directory that another writer populated after the observation above.
	if err := os.Remove(plan.Stage); err == nil {
		if marked {
			return c.finishTransferPreparationCleanup(ctx, cleanup, cleanupMarker)
		}
		return atomicfile.SyncDir(filepath.Dir(plan.Stage))
	}
	if _, err := os.Lstat(plan.Path); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("transfer: refusing cleanup after workspace publication")
	}
	gitDir, found, err := c.revParsePath(plan.Stage, "--absolute-git-dir")
	if err != nil || !found {
		return errors.New("transfer: preparation is not the registered operation worktree")
	}
	plan.GitDir = gitDir
	locked, err := os.ReadFile(filepath.Join(gitDir, "locked"))
	if err != nil || strings.TrimSpace(string(locked)) != transferWorktreeReason(plan.OperationID) {
		return errors.New("transfer: preparation lock belongs to another operation")
	}
	if _, err := os.Stat(filepath.Join(gitDir, transferWorktreeMarker)); err == nil {
		if err := c.checkTransferWorktree(plan, plan.Stage); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	} else {
		// worktree add completed, but the process died before recording the
		// marker. No working files are written before that marker is durable.
		entries, err := os.ReadDir(plan.Stage)
		if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
			return errors.New("transfer: unmarked preparation contains unexpected files")
		}
		head, _, err := c.Execute(plan.Stage, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(head) != plan.Head {
			return errors.New("transfer: unmarked preparation HEAD changed")
		}
	}
	// A crash after worktree removal must still know which NEW branch it may
	// delete. Keep this private, exact plan until both removals are durable.
	if err := atomicfile.WriteJSON(cleanupMarker, plan); err != nil {
		return err
	}
	if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: plan.Repository, ctx: ctx, args: []string{"worktree", "remove", "--force", "--force", "--", plan.Stage}}); err != nil {
		return err
	}
	return c.finishTransferPreparationCleanup(ctx, plan, cleanupMarker)
}

func (c *Core) finishTransferPreparationCleanup(ctx context.Context, plan TransferWorktree, marker string) error {
	if plan.Branch != "" {
		// An already removed ref is an idempotent retry. A different current
		// value is a conflict; the compare-and-delete never removes it.
		result, err := c.runSpec(commandSpec{binary: "git", cwd: plan.Repository, ctx: ctx, args: []string{"show-ref", "--verify", "--quiet", "refs/heads/" + plan.Branch}})
		if err != nil {
			return err
		}
		if result.exitCode != 0 && result.exitCode != 1 {
			return errors.New("transfer: cannot verify the reserved branch")
		}
		if result.exitCode == 0 {
			if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: plan.Repository, ctx: ctx, args: []string{"update-ref", "-d", "refs/heads/" + plan.Branch, plan.Head}}); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(plan.Stage))
}

func isTransferPreparationWorktree(path, reason string) bool {
	id, found := strings.CutPrefix(filepath.Base(path), transferWorktreePrefix)
	if !found {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil && reason == transferWorktreeReason(id)
}

// PublishTransferWorktree is called only with accepted activation proof. A
// crash after rename but before Git's back-reference repair is recoverable;
// another directory at the final path is always a conflict, even if empty.
func (c *Core) PublishTransferWorktree(ctx context.Context, plan TransferWorktree) error {
	if err := validateTransferWorktreePlan(plan); err != nil {
		return err
	}
	stage, stageErr := os.Lstat(plan.Stage)
	if stageErr == nil {
		if !stage.IsDir() {
			return errors.New("transfer: preparation path is no longer a directory")
		}
		if err := c.checkTransferWorktree(plan, plan.Stage); err != nil {
			return err
		}
		if err := c.verifyTransferWorktreeFingerprint(ctx, plan, plan.Stage); err != nil {
			return err
		}
		if err := atomicfile.RenameNoReplace(plan.Stage, plan.Path); err != nil {
			return err
		}
	} else if !errors.Is(stageErr, fs.ErrNotExist) {
		return stageErr
	}
	if err := c.checkTransferWorktree(plan, plan.Path); err != nil {
		return err
	}
	if errors.Is(stageErr, fs.ErrNotExist) {
		if err := c.verifyTransferWorktreeFingerprint(ctx, plan, plan.Path); err != nil {
			return err
		}
	}
	if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: plan.Repository, ctx: ctx, args: []string{"worktree", "repair", "--", plan.Path}}); err != nil {
		return err
	}
	// The marker remains for idempotent install retries after unlocking.
	locked, err := os.ReadFile(filepath.Join(plan.GitDir, "locked"))
	if err == nil {
		if strings.TrimSpace(string(locked)) != transferWorktreeReason(plan.OperationID) {
			return errors.New("transfer: worktree lock now belongs to another operation")
		}
		if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: plan.Repository, ctx: ctx, args: []string{"worktree", "unlock", "--", plan.Path}}); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncTransferTree(ctx, plan.GitDir, 1024)
}

func validateTransferWorktreePlan(plan TransferWorktree) error {
	if _, err := uuid.Parse(plan.OperationID); err != nil {
		return errors.New("transfer: invalid workspace operation")
	}
	if !filepath.IsAbs(plan.Repository) || !filepath.IsAbs(plan.Path) || !filepath.IsAbs(plan.GitDir) || !validTransferOID(plan.Head) || plan.Stage != filepath.Join(filepath.Dir(plan.Path), transferWorktreePrefix+plan.OperationID) || plan.Stage == plan.Path {
		return errors.New("transfer: invalid workspace publication")
	}
	if plan.Branch != "" {
		return ValidateBranchName(plan.Branch)
	}
	return nil
}

func (c *Core) checkTransferWorktree(plan TransferWorktree, directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("transfer: workspace was replaced")
	}
	gitDir, found, err := c.revParsePath(directory, "--absolute-git-dir")
	if err != nil {
		return err
	}
	if !found || !SameFilesystemPath(gitDir, plan.GitDir) {
		return errors.New("transfer: workspace belongs to another checkout")
	}
	// Read a bounded marker, and compare every plan coordinate. In particular
	// a path copied from another operation cannot authorize this publication.
	file, err := os.Open(filepath.Join(gitDir, transferWorktreeMarker))
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 16<<10))
	_ = file.Close()
	if err != nil {
		return err
	}
	var recorded TransferWorktree
	if len(data) >= 16<<10 || json.Unmarshal(data, &recorded) != nil || transferWorktreeCoordinates(recorded) != transferWorktreeCoordinates(plan) {
		return errors.New("transfer: workspace publication identity changed")
	}
	head, _, err := c.Execute(directory, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != plan.Head {
		return errors.New("transfer: prepared workspace HEAD changed")
	}
	branch, err := c.run(directory, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return err
	}
	if (branch.exitCode != 0 && branch.exitCode != 1) || strings.TrimSpace(branch.stdout) != plan.Branch || (plan.Branch == "" && branch.exitCode != 1) {
		return errors.New("transfer: prepared workspace branch changed")
	}
	return nil
}

// The marker is written before materialization, so cleanup can recover every
// prepare crash. Only the returned durable activation recipe owns the final
// content fingerprint; never rewrite the marker halfway through preparation.
func transferWorktreeCoordinates(plan TransferWorktree) TransferWorktree {
	plan.Fingerprint = ""
	return plan
}

// syncTransferTree never follows worktree symlinks. Bounds cover both file
// count and bytes; no goroutine/fd is retained for every tracked file.
func syncTransferTree(ctx context.Context, directory string, maxFiles int) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	var directories []string
	count := 0
	var bytes int64
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maxFiles*2+1 {
			return errors.New("transfer: prepared workspace exceeds file limit")
		}
		if entry.IsDir() {
			directories = append(directories, name)
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("transfer: unsupported prepared workspace file")
		}
		file, err := root.Open(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err == nil {
			bytes += info.Size()
			if bytes > transferfiles.MaxTotalBytes {
				err = errors.New("transfer: prepared workspace exceeds byte limit")
			}
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		return err
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := atomicfile.SyncRootDir(root, filepath.FromSlash(directories[i])); err != nil {
			return err
		}
	}
	return nil
}
