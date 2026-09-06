package rollout

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/transferfiles"
)

var errTransferHistoryBoundary = errors.New("codex transfer: reached history boundary")

// FlattenTransferFiles makes each executable paginated session self-contained.
// A fresh native home can resume context from history_base files but does not
// rebuild those prefixes' separate SQLite history projections. A standalone
// rollout lets the provider rebuild its own turn index, including old reverts.
// Only operation scratch is written; native IDs, turn IDs and payloads remain.
// paths maps executable native IDs to their already-validated archive names.
func FlattenTransferFiles(ctx context.Context, destination string, sources []transferfiles.Source, paths map[string]string) ([]transferfiles.Source, error) {
	if !filepath.IsAbs(destination) || len(sources) == 0 || len(sources) > transferfiles.MaxFiles || len(paths) == 0 || len(paths) > transferfiles.MaxFiles {
		return nil, errors.New("codex transfer: invalid materialization inputs")
	}
	byName := make(map[string]transferfiles.Source, len(sources))
	byRollout := make(map[string]transferfiles.Source, len(sources))
	for _, source := range sources {
		_, id := rolloutFileIDs(source.Name)
		if id == "" || !strings.HasPrefix(source.Name, "native/codex/") || !transferfiles.ValidName(source.Name) || byRollout[id].Name != "" {
			return nil, errors.New("codex transfer: invalid materialization file identity")
		}
		byName[source.Name], byRollout[id] = source, source
	}
	ids := make([]string, 0, len(paths))
	for id := range paths {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	needsFlatten := false
	for _, id := range ids {
		source, ok := byName[paths[id]]
		if !ok {
			return nil, errors.New("codex transfer: executable history is missing")
		}
		meta, err := readTransferMeta(ctx, source, id)
		if err != nil {
			return nil, err
		}
		needsFlatten = needsFlatten || meta.HistoryBase != nil
	}
	if !needsFlatten {
		return sources, nil
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished {
			_ = os.RemoveAll(destination)
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	result := make([]transferfiles.Source, 0, len(ids))
	var readBytes, writtenBytes int64
	for _, id := range ids {
		source := byName[paths[id]]
		meta, err := readTransferMeta(ctx, source, id)
		if err != nil {
			return nil, err
		}
		if meta.HistoryBase == nil {
			result = append(result, source)
			continue
		}
		if meta.HistoryMode != HistoryModePaginated {
			return nil, errors.New("codex transfer: unsupported history prefix format")
		}
		name := strings.TrimSuffix(source.Name, ".zst")
		if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(name)), 0o700); err != nil {
			return nil, err
		}
		if err := flattenTransferSession(ctx, root, name, id, source, byRollout, &readBytes, &writtenBytes); err != nil {
			return nil, err
		}
		result = append(result, transferfiles.Source{Root: destination, Path: name, Name: name})
	}
	if err := atomicfile.SyncDir(destination); err != nil {
		return nil, err
	}
	finished = true
	return result, nil
}

func flattenTransferSession(ctx context.Context, root *os.Root, name, id string, source transferfiles.Source, index map[string]transferfiles.Source, readBytes, writtenBytes *int64) error {
	out, err := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	writer := bufio.NewWriterSize(out, scanBufferSize)
	var ordinal uint64
	var size int64
	write := func(record map[string]json.RawMessage) error {
		record["ordinal"] = json.RawMessage(strconv.FormatUint(ordinal, 10))
		ordinal++
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		size += int64(len(data) + 1)
		*writtenBytes += int64(len(data) + 1)
		if size > transferfiles.MaxFileBytes || *writtenBytes > transferfiles.MaxTotalBytes {
			return errors.New("codex transfer: materialized history exceeds transfer limits")
		}
		if _, err := writer.Write(data); err != nil {
			return err
		}
		return writer.WriteByte('\n')
	}
	// Put the current authoritative header first. Historical metadata headers
	// are control records, not conversation content, and must not reinstall a
	// prefix dependency after this session becomes self-contained.
	err = walkTransferRecords(ctx, source, func(line scanLine) error {
		env, ok := decodeEnvelope(line.Data)
		if !ok || env.Type != typeSessionMeta {
			return nil
		}
		if _, ok := decodeSessionMeta(env, id); !ok {
			return nil
		}
		var record, payload map[string]json.RawMessage
		if err := json.Unmarshal(line.Data, &record); err != nil {
			return err
		}
		if err := json.Unmarshal(record["payload"], &payload); err != nil {
			return err
		}
		delete(payload, "history_base")
		if raw, ok := payload["subagent_history_start_ordinal"]; ok && string(raw) != "null" {
			var original uint64
			if err := json.Unmarshal(raw, &original); err != nil {
				return err
			}
			start, err := flattenedTransferBoundary(ctx, source, index, original, readBytes)
			if err != nil {
				return err
			}
			payload["subagent_history_start_ordinal"] = json.RawMessage(strconv.FormatUint(start, 10))
		}
		record["payload"], _ = json.Marshal(payload)
		if err := write(record); err != nil {
			return err
		}
		return errTransferHistoryBoundary
	})
	if !errors.Is(err, errTransferHistoryBoundary) {
		if err != nil {
			return err
		}
		return ErrSessionMetaNotFound
	}
	err = walkTransferVisibleHistory(ctx, source, index, nil, 0, readBytes, func(line scanLine) error {
		var record map[string]json.RawMessage
		if err := json.Unmarshal(line.Data, &record); err != nil {
			return err
		}
		var kind string
		if err := json.Unmarshal(record["type"], &kind); err != nil {
			return err
		}
		if kind == string(typeSessionMeta) {
			return nil
		}
		return write(record)
	})
	if err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return atomicfile.SyncRootDir(root, filepath.Dir(filepath.FromSlash(name)))
}

// Child context before this boundary must remain outside its projected turns.
// Removing inherited metadata changes ordinals, so keeping the old boundary
// would hide the child's first own records (or make native resume refuse it).
func flattenedTransferBoundary(ctx context.Context, source transferfiles.Source, index map[string]transferfiles.Source, original uint64, readBytes *int64) (uint64, error) {
	if original == 0 {
		return 0, nil
	}
	boundary, seen := uint64(1), uint64(0) // the new standalone header
	stop := errors.New("codex transfer: found subagent boundary")
	err := walkTransferVisibleHistory(ctx, source, index, nil, 0, readBytes, func(line scanLine) error {
		var row struct {
			Ordinal uint64 `json:"ordinal"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(line.Data, &row); err != nil {
			return err
		}
		if row.Ordinal >= original {
			return stop
		}
		seen = row.Ordinal + 1
		if row.Type != string(typeSessionMeta) {
			boundary++
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return 0, err
	}
	if seen != original {
		return 0, errors.New("codex transfer: subagent inherited history is incomplete")
	}
	return boundary, nil
}

// Walk only the retained prefix at each lineage edge. Both byte and ordinal
// boundaries must agree; a truncated or edited prefix is never rounded forward.
func walkTransferVisibleHistory(ctx context.Context, source transferfiles.Source, index map[string]transferfiles.Source, bound *HistoryBase, depth int, readBytes *int64, visit func(scanLine) error) error {
	if depth > 128 {
		return errors.New("codex transfer: history dependency cycle or excessive depth")
	}
	if bound != nil && bound.EndByteOffset == 0 && bound.EndOrdinalExclusive == 0 {
		return nil
	}
	meta, err := readTransferMeta(ctx, source, "")
	if err != nil {
		return err
	}
	var expected uint64
	if base := meta.HistoryBase; base != nil {
		prefix, ok := index[base.ThreadID]
		if !ok {
			return errors.New("codex transfer: missing history prefix")
		}
		if err := walkTransferVisibleHistory(ctx, prefix, index, base, depth+1, readBytes, visit); err != nil {
			return err
		}
		expected = base.EndOrdinalExclusive
	}
	if bound != nil && (bound.EndByteOffset > uint64(transferfiles.MaxFileBytes) || bound.EndOrdinalExclusive < expected) {
		return errors.New("codex transfer: invalid history prefix boundary")
	}
	err = walkTransferRecords(ctx, source, func(line scanLine) error {
		*readBytes += line.Next - line.Start
		if *readBytes > transferfiles.MaxTotalBytes {
			return errors.New("codex transfer: history traversal exceeds transfer limits")
		}
		if bound != nil && uint64(line.Next) > bound.EndByteOffset {
			return errors.New("codex transfer: history prefix cuts through a record")
		}
		var row struct {
			Ordinal *uint64 `json:"ordinal"`
		}
		if err := json.Unmarshal(line.Data, &row); err != nil {
			return err
		}
		if row.Ordinal == nil || *row.Ordinal != expected {
			return errors.New("codex transfer: paginated history ordinal is missing or inconsistent")
		}
		expected++
		if err := visit(line); err != nil {
			return err
		}
		if bound != nil && uint64(line.Next) == bound.EndByteOffset {
			if expected != bound.EndOrdinalExclusive {
				return errors.New("codex transfer: history byte and ordinal boundaries disagree")
			}
			return errTransferHistoryBoundary
		}
		return nil
	})
	if errors.Is(err, errTransferHistoryBoundary) {
		return nil
	}
	if err != nil {
		return err
	}
	if bound != nil {
		return io.ErrUnexpectedEOF
	}
	return nil
}
