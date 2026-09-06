package rollout

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

// TransferCopy is an independent native graph in operation scratch. IDs maps
// native session AND prefix rollout identities, because after a revert they
// are different coordinates. The caller reserves only executable session IDs.
type TransferCopy struct {
	Files []transferfiles.Source
	IDs   map[string]string
}

// CopyTransferFiles copies a dependency-ordered TransferGraph into a NEW private
// directory. It never opens a destination provider home. The operation ID makes
// identities stable across an interrupted snapshot; publication is the app's
// separately journaled operation. Prefix byte coordinates are recalculated at
// exact source record boundaries, not guessed from re-encoded JSON lengths.
func CopyTransferFiles(ctx context.Context, operationID, destination string, sources []transferfiles.Source) (result TransferCopy, err error) {
	operation, err := uuid.Parse(operationID)
	if err != nil || !filepath.IsAbs(destination) || len(sources) == 0 || len(sources) > transferfiles.MaxFiles {
		return result, errors.New("codex transfer: invalid copy inputs")
	}
	result.IDs = make(map[string]string)
	mapID := func(id string) string {
		if mapped := result.IDs[id]; mapped != "" {
			return mapped
		}
		mapped := uuid.NewSHA1(operation, []byte("codex/"+id)).String()
		result.IDs[id] = mapped
		return mapped
	}
	type coordinate struct {
		file   string
		offset uint64
	}
	type position struct {
		offset uint64
		ready  bool
	}
	positions := make(map[coordinate]position)
	metas := make([]SessionMeta, len(sources))
	fileIDs := make(map[string]bool, len(sources))
	for i, source := range sources {
		filenameSession, id := rolloutFileIDs(source.Path)
		if id == "" || !strings.HasPrefix(source.Name, "native/codex/") || !transferfiles.ValidName(source.Name) || fileIDs[id] {
			return result, errors.New("codex transfer: invalid or duplicate native file")
		}
		fileIDs[id] = true
		meta, err := readTransferMeta(ctx, source, "")
		if err != nil {
			return result, err
		}
		metas[i] = meta
		if meta.HistoryMode != "" && meta.HistoryMode != HistoryModeLegacy && meta.HistoryMode != HistoryModePaginated {
			return result, fmt.Errorf("codex transfer: unsupported history mode %q", meta.HistoryMode)
		}
		mapID(id)
		mapID(filenameSession)
		mapID(meta.SessionID)
		if base := meta.HistoryBase; base != nil {
			positions[coordinate{base.ThreadID, base.EndByteOffset}] = position{}
		}
	}
	if err = os.Mkdir(destination, 0o700); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return result, err
	}
	defer root.Close()
	var writtenTotal int64
	// Distinct streams may reuse call IDs; each file keeps only outstanding
	// collaboration calls. Prefix continuations share that small call ledger.
	calls := make(map[string]bool)
	for i, source := range sources {
		filenameSession, fileID := rolloutFileIDs(source.Path)
		if _, needed := positions[coordinate{fileID, 0}]; needed {
			positions[coordinate{fileID, 0}] = position{ready: true}
		}
		name := strings.TrimSuffix(source.Name, ".zst")
		if filenameSession != fileID {
			name = strings.TrimSuffix(name, filenameSession+"_"+fileID+".jsonl") + result.IDs[filenameSession] + "_" + result.IDs[fileID] + ".jsonl"
		} else {
			name = strings.TrimSuffix(name, fileID+".jsonl") + result.IDs[fileID] + ".jsonl"
		}
		if err = root.MkdirAll(filepath.Dir(filepath.FromSlash(name)), 0o700); err != nil {
			return result, err
		}
		out, openErr := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr != nil {
			return result, openErr
		}
		writer := bufio.NewWriterSize(out, scanBufferSize)
		records := transferRecords{calls: calls, metadataIDs: true, identity: func(id string) string {
			// Provenance outside the selected graph stays historical. Every
			// executable reference was collected before entering this function.
			if mapped := result.IDs[id]; mapped != "" {
				return mapped
			}
			return id
		}, prefix: func(base map[string]any) error {
			id, _ := base["thread_id"].(string)
			number, _ := base["end_byte_offset"].(json.Number)
			offset, parseErr := strconv.ParseUint(string(number), 10, 64)
			if parseErr != nil {
				return errors.New("codex transfer: invalid prefix position")
			}
			mapped, ok := positions[coordinate{id, offset}]
			if !ok || !mapped.ready || result.IDs[id] == "" {
				return errors.New("codex transfer: prefix must precede its continuation at an exact record boundary")
			}
			base["thread_id"], base["end_byte_offset"] = result.IDs[id], mapped.offset
			return nil
		}}
		var outputOffset uint64
		err = walkTransferRecords(ctx, source, func(line scanLine) error {
			encoded, err := records.rewrite(line.Data)
			if err != nil {
				return err
			}
			outputOffset += uint64(len(encoded) + 1)
			writtenTotal += int64(len(encoded) + 1)
			if outputOffset > uint64(transferfiles.MaxFileBytes) || writtenTotal > transferfiles.MaxTotalBytes {
				return errors.New("codex transfer: copied native history exceeds transfer limits")
			}
			if _, err := writer.Write(encoded); err != nil {
				return err
			}
			if err := writer.WriteByte('\n'); err != nil {
				return err
			}
			at := coordinate{fileID, uint64(line.Next)}
			if _, needed := positions[at]; needed {
				positions[at] = position{offset: outputOffset, ready: true}
			}
			return nil
		})
		if err == nil {
			err = writer.Flush()
		}
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return result, err
		}
		if outputOffset == 0 || metas[i].SessionID == "" {
			return result, errors.New("codex transfer: empty native file")
		}
		if err = atomicfile.SyncRootDir(root, filepath.Dir(filepath.FromSlash(name))); err != nil {
			return result, err
		}
		result.Files = append(result.Files, transferfiles.Source{Root: destination, Path: name, Name: name})
	}
	for _, mapped := range positions {
		if !mapped.ready {
			return result, errors.New("codex transfer: history prefix ends outside a complete native record")
		}
	}
	err = atomicfile.SyncDir(destination)
	return result, err
}
