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

// appDataDirName is the app's directory inside a data root — and, under
// the OS config root, the directory holding the user's REAL data, which
// is why `db --file` has to be able to name it.
const appDataDirName = "agent-overflow"

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

// minIDPrefix is the shortest --instance value read as an instance-id
// PREFIX rather than as a relative path. Ids are 8 hex chars, so four is
// half of one — long enough that a directory named for it is a
// contrivance, short enough to be worth typing. Below it, a hex-looking
// word is still a path, because `--instance abc` naming a directory is
// the likelier reading.
const minIDPrefix = 4

// instanceEnv is the default for --instance. An agent driving one
// instance for a whole session exports it once instead of threading the
// flag through every invocation; the flag still wins.
const instanceEnv = "AO_HARNESS_INSTANCE"

// resolveTarget picks the instance to act on:
//
//  1. --instance (or $AO_HARNESS_INSTANCE), read as a full instance id,
//     then as a unique id PREFIX, then as a data root.
//  2. Exactly one LIVE registry row.
//  3. Several live rows, one of which is THIS worktree's default data
//     root — the checkout you are standing in wins over the instance
//     somebody left running in another one.
//  4. This worktree's default data root — the same value `make harness`
//     and the backend's own flag default compute.
//
// Anything still ambiguous is an error listing the candidates, and it
// exits 2: two live instances means the invocation was under-specified,
// not that the harness refused something. Guessing would send a reset at
// the wrong one.
func (e *env) resolveTarget() (target, error) {
	rows, err := e.listInstances()
	if err != nil {
		return target{}, err
	}

	if selector := strings.TrimSpace(e.instance); selector != "" {
		return e.resolveSelector(selector, rows)
	}

	var live []instanceinfo.Instance
	for _, row := range rows {
		if !row.Stale {
			live = append(live, row)
		}
	}
	switch {
	case len(live) == 1:
		return targetFromRow(live[0]), nil
	case len(live) == 0:
		// No instance is running. Name this worktree's default so `up`
		// starts the one this checkout would have, and so every other
		// command's error says which data root it looked in.
		return targetFromDataRoot(instanceinfo.DefaultDataRoot(), rows)
	}
	// Several are live. Before declaring ambiguity, check whether one of
	// them is the instance this CHECKOUT owns: a developer with a soak in
	// one worktree and a harness in another means "mine" every time, and
	// making them type eight hex characters to say so is friction with no
	// safety behind it — the default root is not a guess, it is the same
	// derivation `make harness` used to create it.
	mine := instanceinfo.ID(instanceinfo.DefaultDataRoot())
	for _, row := range live {
		if row.ID == mine {
			return targetFromRow(row), nil
		}
	}
	return target{}, exitCodeError{code: exitUsage, err: fmt.Errorf(
		"%d instances are running and none is this worktree's; name one with --instance (or $%s)%s",
		len(live), instanceEnv, candidateList(live))}
}

// resolveSelector reads an explicit --instance value. Order matters: a
// full id is unambiguous, a unique prefix is the git-style convenience,
// and only then is the value a path.
func (e *env) resolveSelector(selector string, rows []instanceinfo.Instance) (target, error) {
	for i := range rows {
		if rows[i].ID == selector {
			return targetFromRow(rows[i]), nil
		}
	}
	if looksLikeIDPrefix(selector) {
		var matches []instanceinfo.Instance
		for i := range rows {
			if strings.HasPrefix(rows[i].ID, selector) {
				matches = append(matches, rows[i])
			}
		}
		switch len(matches) {
		case 1:
			return targetFromRow(matches[0]), nil
		case 0:
			return target{}, fmt.Errorf("no instance %q in the registry%s", selector, candidateList(rows))
		default:
			return target{}, exitCodeError{code: exitUsage, err: fmt.Errorf(
				"%q matches %d instances; type more of the id%s", selector, len(matches), candidateList(matches))}
		}
	}
	if instanceinfo.ValidID(selector) && !strings.ContainsAny(selector, `/\.`) {
		return target{}, fmt.Errorf("no instance %q in the registry%s", selector, candidateList(rows))
	}
	return targetFromDataRoot(selector, rows)
}

// looksLikeIDPrefix is shape-only, and only decides which NAMESPACE a
// selector is read in: hex, no path punctuation, at least minIDPrefix
// characters. It never asserts a row exists.
func looksLikeIDPrefix(selector string) bool {
	if len(selector) < minIDPrefix || len(selector) > 8 {
		return false
	}
	for _, r := range selector {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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
	t := target{ID: id, DataRoot: abs, DataDir: filepath.Join(abs, appDataDirName)}
	for i := range rows {
		if rows[i].ID == id {
			// A row exists for this root: prefer its recorded paths, which
			// is what a running instance actually opened.
			return targetFromRow(rows[i]), nil
		}
	}
	return t, nil
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
		// The WORKTREE is what actually tells two candidates apart for a
		// human: the data roots are /tmp paths derived from it, and the ids
		// are hashes of those. Without it the list is three columns of
		// noise and one path nobody reads to the end of.
		fmt.Fprintf(&b, "\n  %s  %s  %s  %s  %s", row.ID, row.Mode, state, orUnknown(row.Worktree), row.DataRoot)
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
	e.warnVersionSkew(bs)
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
