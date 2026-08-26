package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

// dialTimeout bounds the WS connect. Local, so anything slower than this
// is a wedged backend rather than a slow network.
const dialTimeout = 10 * time.Second

// target is the instance a command acts on, resolved before anything is
// opened. Row is nil when the target came from a path with no registry
// entry — the common case for `up`, which is about to create one.
type target struct {
	ID       string
	DataRoot string
	DataDir  string
	Row      *instanceinfo.Instance
}

// registryDir is the directory this invocation reads rows from.
func (e *env) registry() (string, error) {
	if strings.TrimSpace(e.registryDir) != "" {
		return e.registryDir, nil
	}
	return instanceinfo.RegistryDir()
}

func (e *env) listInstances() ([]instanceinfo.Instance, error) {
	dir, err := e.registry()
	if err != nil {
		return nil, err
	}
	return instanceinfo.ListIn(dir, nil)
}

// resolveTarget picks the instance to act on:
//
//  1. --instance, read as an instance id when a row carries it and as a
//     data root otherwise.
//  2. Exactly one LIVE registry row.
//  3. This worktree's default data root — the same value `make harness`
//     and the backend's own flag default compute.
//
// Ambiguity is an error listing the candidates. Guessing between two
// live instances would send a reset at the wrong one.
func (e *env) resolveTarget() (target, error) {
	rows, err := e.listInstances()
	if err != nil {
		return target{}, err
	}

	if selector := strings.TrimSpace(e.instance); selector != "" {
		for i := range rows {
			if rows[i].ID == selector {
				return targetFromRow(rows[i]), nil
			}
		}
		if looksLikeInstanceID(selector) && !strings.ContainsAny(selector, `/\.`) {
			return target{}, fmt.Errorf("no instance %q in the registry%s", selector, candidateList(rows))
		}
		return targetFromDataRoot(selector, rows)
	}

	var live []instanceinfo.Instance
	for _, row := range rows {
		if !row.Stale {
			live = append(live, row)
		}
	}
	switch len(live) {
	case 1:
		return targetFromRow(live[0]), nil
	case 0:
		// No instance is running. Name this worktree's default so `up`
		// starts the one this checkout would have, and so every other
		// command's error says which data root it looked in.
		return targetFromDataRoot(instanceinfo.DefaultDataRoot(), rows)
	default:
		return target{}, fmt.Errorf("%d instances are running; name one with --instance%s", len(live), candidateList(live))
	}
}

func targetFromRow(row instanceinfo.Instance) target {
	copied := row
	return target{ID: row.ID, DataRoot: row.DataRoot, DataDir: row.DataDir, Row: &copied}
}

func targetFromDataRoot(path string, rows []instanceinfo.Instance) (target, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return target{}, fmt.Errorf("resolve %q: %w", path, err)
	}
	id := instanceinfo.ID(abs)
	t := target{ID: id, DataRoot: abs, DataDir: filepath.Join(abs, "agent-overflow")}
	for i := range rows {
		if rows[i].ID == id {
			// A row exists for this root: prefer its recorded paths, which
			// is what a running instance actually opened.
			return targetFromRow(rows[i]), nil
		}
	}
	return t, nil
}

// looksLikeInstanceID reports whether a selector has the shape an id
// has. Used only to pick the better error message; resolution itself
// keys on whether a row carries the value.
func looksLikeInstanceID(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func candidateList(rows []instanceinfo.Instance) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(":")
	for _, row := range rows {
		state := "live"
		if row.Stale {
			state = "stale"
		}
		fmt.Fprintf(&b, "\n  %s  %s  %s  %s", row.ID, row.Mode, state, row.DataRoot)
	}
	return b.String()
}

// attach resolves the target, reads its instance file, and connects.
// Every command that talks to a backend starts here.
func (e *env) attach(ctx context.Context) (*harnessclient.Client, target, harnessclient.Bootstrap, error) {
	t, err := e.resolveTarget()
	if err != nil {
		return nil, target{}, harnessclient.Bootstrap{}, err
	}
	bs, err := harnessclient.ReadInstanceFile(t.DataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, t, harnessclient.Bootstrap{}, fmt.Errorf(
				"no live instance at %s (start one with `ao-harness up --data-dir %s`)", t.DataRoot, t.DataRoot)
		}
		return nil, t, harnessclient.Bootstrap{}, err
	}
	if !instanceinfo.ProcessAlive(bs.PID) {
		return nil, t, bs, fmt.Errorf(
			"instance %s names pid %d, which is not running; its data dir is %s (`ao-harness list` prunes the row)",
			t.ID, bs.PID, t.DataDir)
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	client, err := harnessclient.Dial(dialCtx, bs, harnessclient.Options{})
	if err != nil {
		return nil, t, bs, err
	}
	return client, t, bs, nil
}

// withClient runs fn against an attached instance and closes the
// connection afterwards.
func (e *env) withClient(ctx context.Context, fn func(client *harnessclient.Client, t target, bs harnessclient.Bootstrap) error) error {
	client, t, bs, err := e.attach(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	return fn(client, t, bs)
}

// call is the one-shot shape most commands want: attach, invoke, print.
func (e *env) call(ctx context.Context, method string, params ...any) error {
	return e.withClient(ctx, func(client *harnessclient.Client, _ target, _ harnessclient.Bootstrap) error {
		result, err := client.Call(ctx, method, params...)
		if err != nil {
			return err
		}
		return e.writeRawJSON(result)
	})
}
