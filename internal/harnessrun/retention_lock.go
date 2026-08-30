package harnessrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/atomicfile"
)

type retentionLock struct {
	path  string
	token string
}

type retentionLockOwner struct {
	Token    string    `json:"token"`
	PID      int       `json:"pid"`
	Acquired time.Time `json:"acquiredAt"`
}

func (r *ArtifactRegistry) lock(ctx context.Context) (*retentionLock, error) {
	if r == nil {
		return nil, errors.New("artifact registry is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact registry: %w", err)
	}
	path := filepath.Join(r.dir, RegistryLockName)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			token, tokenErr := randomToken()
			if tokenErr != nil {
				return nil, fmt.Errorf("create artifact registry lock identity: %w", errors.Join(tokenErr, os.Remove(path)))
			}
			owner := retentionLockOwner{Token: token, PID: os.Getpid(), Acquired: time.Now().UTC()}
			if ownerErr := atomicfile.WriteJSON(filepath.Join(path, "owner.json"), owner); ownerErr != nil {
				return nil, fmt.Errorf("publish artifact registry lock: %w", errors.Join(ownerErr, os.Remove(path)))
			}
			return &retentionLock{path: path, token: token}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("acquire artifact registry lock: %w", err)
		}
		if owner, ownerErr := readRetentionLockOwner(filepath.Join(path, "owner.json")); ownerErr == nil && !lockProcessAlive(owner.PID) {
			// A dead owner is the only condition that permits recovery. A
			// malformed or unreadable owner is retained and eventually times
			// out, rather than risking removal of a live holder.
			if removeErr := os.Remove(filepath.Join(path, "owner.json")); removeErr == nil || errors.Is(removeErr, fs.ErrNotExist) {
				if removeErr = os.Remove(path); removeErr == nil || errors.Is(removeErr, fs.ErrNotExist) {
					continue
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("acquire artifact registry lock: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func readRetentionLockOwner(path string) (retentionLockOwner, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return retentionLockOwner{}, err
	}
	var owner retentionLockOwner
	if err := decodeStrict(data, &owner); err != nil {
		return retentionLockOwner{}, err
	}
	if owner.Token == "" || owner.PID <= 0 || owner.Acquired.IsZero() {
		return retentionLockOwner{}, errors.New("malformed lock owner")
	}
	return owner, nil
}

func (l *retentionLock) release() error {
	if l == nil {
		return nil
	}
	owner, err := readRetentionLockOwner(filepath.Join(l.path, "owner.json"))
	if err != nil {
		return fmt.Errorf("release artifact registry lock: %w", err)
	}
	if owner.Token != l.token {
		return errors.New("release artifact registry lock: owner changed")
	}
	if err := os.Remove(filepath.Join(l.path, "owner.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release artifact registry lock owner: %w", err)
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release artifact registry lock: %w", err)
	}
	return nil
}
