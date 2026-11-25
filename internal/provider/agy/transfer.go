package agy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/transferfiles"
)

// agyTransferCopyNamespace scopes deterministic UUIDv5 derivation for copied
// Antigravity transfers so retried copy operations reuse the same session ID.
var agyTransferCopyNamespace = uuid.MustParse("9b21e5b4-19fd-4f76-9f64-6c5f5d3209a1")

// CopiedTransfer is the materialized independent native Antigravity session for
// a copy transfer.
type CopiedTransfer struct {
	SessionID string
	Files     []transferfiles.Source
}

// TransferFiles snapshots `<appDataDir>/conversations/<sessionID>.db` into
// `scratchDir` using SQLite VACUUM INTO (WAL-safe) and collects any companion
// regular files under `<appDataDir>/brain/<sessionID>/`.
func TransferFiles(ctx context.Context, appDataDir, sessionID, scratchDir string) ([]transferfiles.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return nil, fmt.Errorf("agy transfer: invalid session id %q: %w", sessionID, err)
	}
	if strings.TrimSpace(appDataDir) == "" {
		return nil, errors.New("agy transfer: appDataDir is required")
	}

	srcPath, err := conversationDBPath(appDataDir, sessionID)
	if err != nil {
		return nil, fmt.Errorf("agy transfer: %w", err)
	}
	srcDB, err := openConversationDBReadOnly(srcPath)
	if err != nil {
		return nil, fmt.Errorf("agy transfer: open conversation %s: %w", sessionID, err)
	}
	defer func() { _ = srcDB.Close() }()

	if err := verifyConversationSchema(srcDB); err != nil {
		return nil, fmt.Errorf("agy transfer: %w", err)
	}

	relDB := path.Join("conversations", sessionID+".db")
	snapPath := filepath.Join(scratchDir, filepath.FromSlash(relDB))
	if err := os.MkdirAll(filepath.Dir(snapPath), 0o700); err != nil {
		return nil, fmt.Errorf("agy transfer: create snapshot dir: %w", err)
	}
	_ = os.Remove(snapPath)
	if _, err := srcDB.ExecContext(ctx, `VACUUM INTO ?`, snapPath); err != nil {
		_ = os.Remove(snapPath)
		return nil, fmt.Errorf("agy transfer: snapshot conversation %s: %w", sessionID, err)
	}

	sources := []transferfiles.Source{{
		Root: scratchDir,
		Path: relDB,
		Name: path.Join("native/agy", relDB),
	}}

	brainSources, err := collectBrainTransferFiles(ctx, appDataDir, sessionID)
	if err != nil {
		return nil, err
	}
	sources = append(sources, brainSources...)
	return sources, nil
}

// CopyTransferFiles materializes an independent Antigravity session identity
// for a copy transfer: deterministically derives a new UUID from
// (operationID, sessionID), rewrites trajectory_meta in a fresh SQLite copy,
// and remaps companion brain/<id>/ paths to the new UUID.
func CopyTransferFiles(
	ctx context.Context,
	operationID, sessionID, destDir string,
	sources []transferfiles.Source,
) (CopiedTransfer, error) {
	if err := ctx.Err(); err != nil {
		return CopiedTransfer{}, err
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return CopiedTransfer{}, fmt.Errorf("agy transfer copy: invalid session id %q: %w", sessionID, err)
	}
	newSessionID := uuid.NewSHA1(agyTransferCopyNamespace, []byte("session:"+operationID+":"+sessionID)).String()
	newTrajID := uuid.NewSHA1(agyTransferCopyNamespace, []byte("trajectory:"+operationID+":"+sessionID)).String()

	oldDBName := path.Join("native/agy/conversations", sessionID+".db")
	newDBRel := path.Join("conversations", newSessionID+".db")
	newDBName := path.Join("native/agy", newDBRel)
	oldBrainPrefix := path.Join("native/agy/brain", sessionID) + "/"
	newBrainPrefix := path.Join("native/agy/brain", newSessionID) + "/"

	out := make([]transferfiles.Source, 0, len(sources))
	foundDB := false
	for _, src := range sources {
		if src.Name == oldDBName {
			srcDBPath := filepath.Join(src.Root, filepath.FromSlash(src.Path))
			dstDBPath := filepath.Join(destDir, filepath.FromSlash(newDBRel))
			if err := cloneAndRewriteTransferDB(ctx, srcDBPath, dstDBPath, newTrajID, newSessionID); err != nil {
				return CopiedTransfer{}, err
			}
			out = append(out, transferfiles.Source{
				Root: destDir,
				Path: newDBRel,
				Name: newDBName,
			})
			foundDB = true
			continue
		}
		if strings.HasPrefix(src.Name, oldBrainPrefix) {
			remapped := src
			remapped.Name = newBrainPrefix + strings.TrimPrefix(src.Name, oldBrainPrefix)
			out = append(out, remapped)
			continue
		}
		return CopiedTransfer{}, fmt.Errorf("agy transfer copy: unexpected native source %q", src.Name)
	}
	if !foundDB {
		return CopiedTransfer{}, errors.New("agy transfer copy: conversation database missing from snapshot")
	}
	return CopiedTransfer{
		SessionID: newSessionID,
		Files:     out,
	}, nil
}

// ValidateStagedTransferFiles verifies a staged incoming `native/agy` directory
// on the destination computer: checks that `conversations/<sessionID>.db`
// passes schema verification and matches `cascade_id == sessionID`, and returns
// the exact expected native transfer file list.
func ValidateStagedTransferFiles(ctx context.Context, stageAgyDir, sessionID string) ([]transferfiles.Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return nil, errors.New("invalid Antigravity session identity")
	}
	relDB := path.Join("conversations", sessionID+".db")
	dbPath := filepath.Join(stageAgyDir, filepath.FromSlash(relDB))
	db, err := openConversationDBReadOnly(dbPath)
	if err != nil {
		return nil, errors.New("antigravity conversation database is missing or unreadable")
	}
	defer func() { _ = db.Close() }()

	if err := verifyConversationSchema(db); err != nil {
		return nil, fmt.Errorf("invalid Antigravity conversation database: %w", err)
	}
	var cascadeID string
	if err := db.QueryRowContext(ctx, `SELECT cascade_id FROM trajectory_meta LIMIT 1`).Scan(&cascadeID); err != nil {
		return nil, errors.New("antigravity conversation metadata is missing")
	}
	if strings.TrimSpace(cascadeID) != sessionID {
		return nil, errors.New("antigravity conversation database does not match the transfer session")
	}

	sources := []transferfiles.Source{{
		Root: stageAgyDir,
		Path: relDB,
		Name: path.Join("native/agy", relDB),
	}}
	brainSources, err := collectBrainTransferFiles(ctx, stageAgyDir, sessionID)
	if err != nil {
		return nil, err
	}
	sources = append(sources, brainSources...)
	return sources, nil
}

func cloneAndRewriteTransferDB(ctx context.Context, srcPath, dstPath, newTrajID, newSessionID string) error {
	srcDB, err := openConversationDBReadOnly(srcPath)
	if err != nil {
		return fmt.Errorf("agy transfer copy: open snapshot: %w", err)
	}
	defer func() { _ = srcDB.Close() }()

	if err := verifyConversationSchema(srcDB); err != nil {
		return fmt.Errorf("agy transfer copy: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
		return fmt.Errorf("agy transfer copy: create destination dir: %w", err)
	}
	_ = os.Remove(dstPath)
	if _, err := srcDB.ExecContext(ctx, `VACUUM INTO ?`, dstPath); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("agy transfer copy: clone db: %w", err)
	}

	if err := rewriteTransferTrajectoryMeta(ctx, dstPath, newTrajID, newSessionID); err != nil {
		_ = os.Remove(dstPath)
		return err
	}
	return nil
}

func rewriteTransferTrajectoryMeta(ctx context.Context, dstPath, newTrajID, newSessionID string) error {
	dstDB, err := sql.Open("sqlite", sqliteFileDSN(dstPath, "_pragma=busy_timeout(200)"))
	if err != nil {
		return fmt.Errorf("agy transfer copy: open cloned db: %w", err)
	}
	defer func() { _ = dstDB.Close() }()

	if _, err := dstDB.ExecContext(ctx, `UPDATE trajectory_meta SET trajectory_id = ?, cascade_id = ?`, newTrajID, newSessionID); err != nil {
		return fmt.Errorf("agy transfer copy: rewrite trajectory_meta: %w", err)
	}
	return nil
}

func collectBrainTransferFiles(ctx context.Context, rootDir, sessionID string) ([]transferfiles.Source, error) {
	brainDir := filepath.Join(rootDir, "brain", sessionID)
	info, err := os.Lstat(brainDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil
	}

	var relPaths []string
	walkErr := filepath.WalkDir(brainDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".system_generated" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(rootDir, p)
		if err != nil {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		name := path.Join("native/agy", slashRel)
		if !transferfiles.ValidName(name) {
			return fmt.Errorf("invalid Antigravity brain path %q", slashRel)
		}
		relPaths = append(relPaths, slashRel)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(relPaths)
	out := make([]transferfiles.Source, 0, len(relPaths))
	for _, rel := range relPaths {
		out = append(out, transferfiles.Source{
			Root: rootDir,
			Path: rel,
			Name: path.Join("native/agy", rel),
		})
	}
	return out, nil
}
