package rollout

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

	"github.com/klauspost/compress/zstd"

	"agent-overflow/internal/transferfiles"
)

// TransferReference comes from a metadata-only thread/read on the provider.
// The active rollout filename may have a DIFFERENT id after thread/revert.
// Callers include the root and any provider-owned child sessions to preserve.
type TransferReference struct{ SessionID, Path string }

// TransferFileSession identifies a previously validated graph member for the
// destination's retired-file replacement policy. Historical prefix filenames
// need not equal their owning native session ID after a revert.
func TransferFileSession(ctx context.Context, source transferfiles.Source) (string, error) {
	meta, err := readTransferMeta(ctx, source, "")
	return meta.SessionID, err
}

// TransferFiles collects the referenced rollouts and their immutable history
// prefixes. This is targeted export, never a fallback for the session catalog:
// primary paths come from the provider, and filename discovery resolves only
// history_base rollout IDs, which may not have thread-index rows of their own.
func TransferFiles(ctx context.Context, codexHome string, references []TransferReference) ([]transferfiles.Source, error) {
	_, files, err := collectTransferFiles(ctx, codexHome, references, nil)
	return files, err
}

// TransferGraph follows native collaboration references as well as history
// prefixes. thread/fork does NOT clone children, and thread/list's spawn-edge
// index can be empty after copying native files into a new home. The durable
// records are necessary even when the provider's current child list is empty.
// resolve must be a metadata-only provider thread/read, never a directory guess.
func TransferGraph(ctx context.Context, codexHome string, root TransferReference, resolve func(context.Context, string) (TransferReference, error)) ([]TransferReference, []transferfiles.Source, error) {
	if resolve == nil {
		return nil, nil, errors.New("codex transfer: missing native resolver")
	}
	return collectTransferFiles(ctx, codexHome, []TransferReference{root}, resolve)
}

func collectTransferFiles(ctx context.Context, codexHome string, references []TransferReference, resolve func(context.Context, string) (TransferReference, error)) ([]TransferReference, []transferfiles.Source, error) {
	if strings.TrimSpace(codexHome) == "" || len(references) == 0 || len(references) > transferfiles.MaxFiles {
		return nil, nil, errors.New("codex transfer: missing home or session references")
	}
	var sources []transferfiles.Source
	visited := make(map[string]bool)
	active := make(map[string]bool)
	var index map[string][]string
	var collect func(string, string, int) error
	collect = func(file, sessionID string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 128 {
			return errors.New("codex transfer: history dependency chain is too deep")
		}
		source, err := transferSource(codexHome, file)
		if err != nil {
			return err
		}
		source, err = existingTransferSource(source)
		if err != nil {
			return err
		}
		key := source.Name
		if active[key] {
			return errors.New("codex transfer: history dependency cycle")
		}
		if visited[key] {
			return nil
		}
		if len(sources) >= transferfiles.MaxFiles {
			return errors.New("codex transfer: too many native files")
		}
		meta, err := readTransferMeta(ctx, source, sessionID)
		if err != nil {
			return err
		}
		if meta.HistoryMode != "" && meta.HistoryMode != HistoryModeLegacy && meta.HistoryMode != HistoryModePaginated {
			return fmt.Errorf("codex transfer: unsupported history mode %q", meta.HistoryMode)
		}
		active[key] = true
		if base := meta.HistoryBase; base != nil {
			if !looksLikeUUID(base.ThreadID) {
				return errors.New("codex transfer: invalid history prefix identity")
			}
			if index == nil {
				index, err = transferRolloutIndex(ctx, codexHome)
				if err != nil {
					return err
				}
			}
			paths := index[strings.ToLower(base.ThreadID)]
			if len(paths) != 1 {
				return fmt.Errorf("codex transfer: history prefix %s is missing or ambiguous", base.ThreadID)
			}
			if err := collect(paths[0], "", depth+1); err != nil {
				return err
			}
		}
		delete(active, key)
		visited[key] = true
		sources = append(sources, source)
		return nil
	}
	// Copy the slice: discoveries must not mutate the caller's backing array.
	references = append([]TransferReference(nil), references...)
	known := make(map[string]bool, len(references))
	for _, ref := range references {
		known[ref.SessionID] = true
	}
	var pending []string
	records := transferRecords{identity: func(id string) string {
		if !known[id] && len(known) <= transferfiles.MaxFiles {
			known[id] = true
			pending = append(pending, id)
		}
		return id
	}}
	var decodedBytes int64
	filesByRollout := make(map[string]transferfiles.Source)
	for n := 0; n < len(references); n++ {
		ref := references[n]
		if !looksLikeUUID(ref.SessionID) {
			return nil, nil, errors.New("codex transfer: invalid session identity")
		}
		firstNew := len(sources)
		if err := collect(ref.Path, ref.SessionID, 0); err != nil {
			return nil, nil, err
		}
		if resolve == nil {
			continue
		}
		for _, source := range sources[firstNew:] {
			_, id := rolloutFileIDs(source.Name)
			filesByRollout[id] = source
		}
		current, err := transferSource(codexHome, ref.Path)
		if err != nil {
			return nil, nil, err
		}
		current, err = existingTransferSource(current)
		if err != nil {
			return nil, nil, err
		}
		meta, err := readTransferMeta(ctx, current, ref.SessionID)
		if err != nil {
			return nil, nil, err
		}
		if meta.HistoryBase != nil {
			// Reverted-away collaboration calls are outside this conversation.
			// Read the visible lineage, not every byte in shared prefix files.
			err = walkTransferVisibleHistory(ctx, current, filesByRollout, nil, 0, &decodedBytes, func(line scanLine) error {
				_, err := records.rewrite(line.Data)
				return err
			})
		} else {
			err = walkTransferRecords(ctx, current, func(line scanLine) error {
				decodedBytes += line.Next - line.Start
				if decodedBytes > transferfiles.MaxTotalBytes {
					return errors.New("codex transfer: native graph exceeds the transfer limit")
				}
				_, err := records.rewrite(line.Data)
				return err
			})
		}
		if err != nil {
			return nil, nil, err
		}
		if len(known) > transferfiles.MaxFiles {
			return nil, nil, errors.New("codex transfer: too many native sessions")
		}
		for _, id := range pending {
			child, err := resolve(ctx, id)
			if err != nil {
				return nil, nil, fmt.Errorf("codex transfer: dependent session %s: %w", id, err)
			}
			if child.SessionID != id {
				return nil, nil, errors.New("codex transfer: resolver returned another session")
			}
			references = append(references, child)
		}
		pending = pending[:0]
	}
	return references, sources, nil
}

func existingTransferSource(source transferfiles.Source) (transferfiles.Source, error) {
	root, err := os.OpenRoot(source.Root)
	if err != nil {
		return source, err
	}
	defer root.Close()
	plain := strings.TrimSuffix(source.Path, ".zst")
	info, err := root.Lstat(filepath.FromSlash(plain))
	if errors.Is(err, fs.ErrNotExist) {
		source.Path = plain + ".zst"
		source.Name = strings.TrimSuffix(source.Name, ".zst") + ".zst"
		info, err = root.Lstat(filepath.FromSlash(source.Path))
	} else {
		source.Path = plain
		source.Name = strings.TrimSuffix(source.Name, ".zst")
	}
	if err != nil {
		return source, err
	}
	if !info.Mode().IsRegular() {
		return source, errors.New("codex transfer: rollout is not a regular file")
	}
	return source, nil
}

func transferSource(home, file string) (transferfiles.Source, error) {
	contained, err := PathInHome(home, file)
	if errors.Is(err, ErrOutsideCodexHome) {
		// Providers can canonicalize an injected home (/var -> /private/var
		// on macOS). Resolve only the trusted home, never an untrusted target
		// outside it, then repeat the same lexical containment gate.
		if canonical, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
			if resolved, checkErr := PathInHome(canonical, file); checkErr == nil {
				home, contained, err = canonical, resolved, nil
			}
		}
	}
	if err != nil {
		return transferfiles.Source{}, err
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return transferfiles.Source{}, err
	}
	relative, err := filepath.Rel(absoluteHome, contained)
	if err != nil {
		return transferfiles.Source{}, err
	}
	parts := strings.SplitN(filepath.ToSlash(relative), "/", 2)
	if len(parts) != 2 || (parts[0] != "sessions" && parts[0] != "archived_sessions") || !transferfiles.ValidName(parts[1]) || SessionIDFromPath(strings.TrimSuffix(file, ".zst")) == "" {
		return transferfiles.Source{}, errors.New("codex transfer: unsupported rollout location")
	}
	return transferfiles.Source{Root: filepath.Join(absoluteHome, parts[0]), Path: parts[1], Name: "native/codex/" + strings.Join(parts, "/")}, nil
}

func readTransferMeta(ctx context.Context, source transferfiles.Source, sessionID string) (SessionMeta, error) {
	root, err := os.OpenRoot(source.Root)
	if err != nil {
		return SessionMeta{}, err
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(source.Path))
	if err != nil {
		return SessionMeta{}, err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(source.Path, ".zst") {
		decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			return SessionMeta{}, err
		}
		defer decoder.Close()
		reader = decoder
	}
	scanner := newScanner(io.LimitReader(reader, headScanBytes), 0, DefaultMaxLineBytes, headScanBufferSize)
	var found SessionMeta
	filenameID := SessionIDFromPath(strings.TrimSuffix(source.Path, ".zst"))
	for range headScanLines {
		if err := ctx.Err(); err != nil {
			return SessionMeta{}, err
		}
		line, err := scanner.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SessionMeta{}, err
		}
		env, ok := decodeEnvelope(line.Data)
		if !ok || env.Type != typeSessionMeta {
			continue
		}
		id := sessionID
		if id == "" {
			var payload sessionMetaPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				return SessionMeta{}, err
			}
			id = payload.ID
			if id == "" {
				id = payload.SessionID
			}
		}
		if meta, ok := decodeSessionMeta(env, id); ok {
			if sessionID != "" || meta.SessionID == filenameID {
				return meta, nil
			}
			// A reverted rollout's filename differs from its native ID.
			// Its first header is authoritative; later headers can be
			// inherited source provenance from an earlier native fork.
			if found.SessionID == "" {
				found = meta
			}
		}
	}
	if found.SessionID == "" {
		return SessionMeta{}, ErrSessionMetaNotFound
	}
	return found, nil
}

// One bounded filename pass, only if a history prefix exists. No transcript
// bodies or index credentials are read here. Plain files win over compressed
// siblings, matching Codex's own compression/materialization contract.
func transferRolloutIndex(ctx context.Context, home string) (map[string][]string, error) {
	index := make(map[string][]string)
	entries := 0
	for _, directory := range []string{"sessions", "archived_sessions"} {
		rootPath := filepath.Join(home, directory)
		root, err := os.OpenRoot(rootPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			entries++
			if entries > 250_000 {
				return errors.New("codex transfer: rollout directory exceeds the discovery limit")
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			_, id := rolloutFileIDs(name)
			if id == "" {
				return nil
			}
			if strings.HasSuffix(name, ".zst") {
				if _, err := root.Stat(filepath.FromSlash(strings.TrimSuffix(name, ".zst"))); err == nil {
					return nil
				} else if !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			index[strings.ToLower(id)] = append(index[strings.ToLower(id)], filepath.Join(rootPath, filepath.FromSlash(name)))
			return nil
		})
		_ = root.Close()
		if err != nil {
			return nil, err
		}
	}
	return index, nil
}
