package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// processTemplate is the migrated image NewFromTemplate copies.
var processTemplate = newMigratedTemplate(buildMigratedImage)

// NewFromTemplate creates a database at path and opens it. The result is
// what New returns for a missing file: the same schema, migration
// version, deferred watermark and file-level PRAGMAs, and an identity of
// its own. Only the migration rows' applied times differ. The file is a
// copy of an empty database the migration chain ran on once per process,
// so the chain is not replayed per database.
//
// The first call builds that image in a temporary directory beside path,
// keeps it in memory for the life of the process and removes the
// directory, so no template file outlives the call. A failed build is
// returned and the next call builds again. path must not exist, nor may
// a WAL or rollback journal beside it. The file is created with mode
// 0600 and is removed again if the open fails.
func NewFromTemplate(ctx context.Context, path string) (*Store, error) {
	return processTemplate.open(ctx, path)
}

// migratedTemplate builds a migrated image on first use and copies it.
// lock is a one-slot semaphore rather than a mutex so a caller waiting
// for another's build can give up when its context ends.
type migratedTemplate struct {
	lock  chan struct{}
	image []byte
	build func(ctx context.Context, dir string) ([]byte, error)
}

func newMigratedTemplate(build func(context.Context, string) ([]byte, error)) *migratedTemplate {
	return &migratedTemplate{lock: make(chan struct{}, 1), build: build}
}

func (t *migratedTemplate) open(ctx context.Context, path string) (*Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := refuseSidecars(path); err != nil {
		return nil, err
	}
	image, err := t.load(ctx, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if err := writeImage(path, image); err != nil {
		return nil, err
	}
	s, err := NewWithOptions(path, Options{Context: ctx})
	if err != nil {
		return nil, errors.Join(err, removeDatabaseFiles(path))
	}
	return s, nil
}

// load returns the image, building it in a directory under parent when
// no earlier call succeeded.
func (t *migratedTemplate) load(ctx context.Context, parent string) ([]byte, error) {
	select {
	case t.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-t.lock }()
	if t.image != nil {
		return t.image, nil
	}
	dir, err := os.MkdirTemp(parent, ".store-template-")
	if err != nil {
		return nil, fmt.Errorf("store: create template directory: %w", err)
	}
	image, err := t.build(ctx, dir)
	if removeErr := os.RemoveAll(dir); removeErr != nil {
		if err != nil {
			return nil, errors.Join(err, fmt.Errorf("store: remove template directory: %w", removeErr))
		}
		// The image is complete; only the scratch directory is left.
		log.Printf("store: remove template directory %s: %v", dir, removeErr)
	}
	if err != nil {
		return nil, err
	}
	t.image = image
	return image, nil
}

// buildMigratedImage migrates a new database in dir and returns its file.
// The identity row is deleted first so each copy mints its own when it
// opens (ensureStoreIdentity). Close's TRUNCATE checkpoint moves the WAL
// into the main file; a WAL left with content would be lost by the copy,
// so the build fails instead.
func buildMigratedImage(ctx context.Context, dir string) ([]byte, error) {
	path := filepath.Join(dir, "template.db")
	s, err := NewWithOptions(path, Options{Context: ctx})
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`DELETE FROM store_meta`)
	if err != nil {
		err = fmt.Errorf("store: clear template identity: %w", err)
	}
	if err := errors.Join(err, s.Close()); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		return nil, fmt.Errorf("store: template WAL kept %d bytes after close", info.Size())
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("store: stat template WAL: %w", err)
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("store: read template: %w", err)
	}
	return image, nil
}

// refuseSidecars fails when a WAL or rollback journal sits at path: SQLite
// would apply it to the new file.
func refuseSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("store: %s%s already exists", path, suffix)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: check %s%s: %w", path, suffix, err)
		}
	}
	return nil
}

// writeImage creates path, which must not exist, with the image's bytes
// and syncs it, so a later checkpoint never lands on unwritten pages.
func writeImage(path string, image []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("store: create database from template: %w", err)
	}
	_, err = file.Write(image)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.Join(fmt.Errorf("store: write database from template: %w", err), removeDatabaseFiles(path))
	}
	return nil
}
