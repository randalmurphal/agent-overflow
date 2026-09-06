package sessionfork

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

// TransferCopy preserves the native message UUIDs, as Claude's own latest-turn
// --fork-session does. Child IDs are scoped by the root session directory; the
// entire sidecar tree moves under the new root. Arbitrary-cut forks still use
// forksession.go's branch transform and message remapping.
type TransferCopy struct {
	SessionID string
	Files     []transferfiles.Source
}

// CopyTransferFiles clones one complete, quiescent native session into NEW
// operation scratch. The destination installer relocates the root AND sidecars
// into its own workspace slug. Never call this with a live provider directory.
func CopyTransferFiles(ctx context.Context, operationID, sessionID, destination string, sources []transferfiles.Source) (result TransferCopy, err error) {
	return CopyTransferFilesAt(ctx, TransferCopyCut{OperationID: operationID, SessionID: sessionID, Destination: destination}, sources)
}

type TransferCopyCut struct {
	OperationID string
	SessionID   string
	Destination string
	ThroughUUID string
}

// CopyTransferFilesAt materializes a lazy fork's pinned prefix without changing
// message identities. The caller resolves ThroughUUID with the same resume
// filters as a local fork start. Never silently extend a missing pin to the tail.
func CopyTransferFilesAt(ctx context.Context, cut TransferCopyCut, sources []transferfiles.Source) (result TransferCopy, err error) {
	operationID, sessionID, destination := cut.OperationID, cut.SessionID, cut.Destination
	if len(cut.ThroughUUID) > 4096 || strings.ContainsAny(cut.ThroughUUID, "\x00\r\n") {
		return result, errors.New("claude transfer: invalid fork cursor")
	}
	operation, err := uuid.Parse(operationID)
	if _, parseErr := uuid.Parse(sessionID); err != nil || parseErr != nil || !filepath.IsAbs(destination) || len(sources) == 0 || len(sources) > transferfiles.MaxFiles {
		return result, errors.New("claude transfer: invalid copy inputs")
	}
	result.SessionID = uuid.NewSHA1(operation, []byte("claude/"+sessionID)).String()
	first := strings.TrimPrefix(sources[0].Name, "native/claude/")
	if first == sources[0].Name || path.Base(first) != sessionID+".jsonl" || !transferfiles.ValidName(first) {
		return result, errors.New("claude transfer: missing root transcript")
	}
	slug := path.Dir(first)
	if strings.Contains(slug, "/") {
		return result, errors.New("claude transfer: invalid project slug")
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
	var total int64
	buffer := make([]byte, 128<<10)
	seen := make(map[string]bool, len(sources))
	for i, source := range sources {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		relative := strings.TrimPrefix(source.Name, "native/claude/"+slug+"/")
		if !transferfiles.ValidName(source.Name) || seen[source.Name] || (relative != sessionID+".jsonl" && !strings.HasPrefix(relative, sessionID+"/")) {
			return result, errors.New("claude transfer: file is outside the selected session")
		}
		seen[source.Name] = true
		name := path.Join("native/claude", slug, result.SessionID+strings.TrimPrefix(relative, sessionID))
		if err = root.MkdirAll(filepath.Dir(filepath.FromSlash(name)), 0o700); err != nil {
			return result, err
		}
		out, openErr := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr != nil {
			return result, openErr
		}
		writer := bufio.NewWriterSize(out, 128<<10)
		var size int64
		cursor := ""
		if i == 0 {
			cursor = cut.ThroughUUID
		}
		err = copyTransferNative(ctx, source, sessionID, result.SessionID, cursor, writer, buffer, &size)
		total += size
		if err == nil && total > transferfiles.MaxTotalBytes {
			err = errors.New("claude transfer: copied native history exceeds transfer limits")
		}
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
		if err = atomicfile.SyncRootDir(root, filepath.Dir(filepath.FromSlash(name))); err != nil {
			return result, err
		}
		result.Files = append(result.Files, transferfiles.Source{Root: destination, Path: name, Name: name})
	}
	err = atomicfile.SyncDir(destination)
	return result, err
}

func copyTransferNative(ctx context.Context, source transferfiles.Source, fromID, toID, throughUUID string, output io.Writer, buffer []byte, size *int64) error {
	root, err := os.OpenRoot(source.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(source.Path))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > transferfiles.MaxFileBytes {
		return errors.New("claude transfer: unsupported native file")
	}
	write := func(data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*size += int64(len(data))
		if *size > transferfiles.MaxFileBytes {
			return errors.New("claude transfer: native file exceeds transfer limit")
		}
		_, err := output.Write(data)
		return err
	}
	if strings.HasSuffix(source.Path, ".jsonl") {
		scanner := bufio.NewScanner(io.LimitReader(file, transferfiles.MaxFileBytes+1))
		scanner.Buffer(buffer, scannerBufMax)
		scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				return i + 1, data[:i], nil
			}
			if atEOF && len(data) > 0 {
				return 0, nil, errors.New("claude transfer: incomplete native record")
			}
			return 0, nil, nil
		})
		for scanner.Scan() {
			data, err := copyTransferRecord(scanner.Bytes(), fromID, toID)
			if err != nil {
				return err
			}
			if err := write(append(data, '\n')); err != nil {
				return err
			}
			if throughUUID != "" {
				var row struct {
					UUID string `json:"uuid"`
				}
				if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
					return err
				}
				if row.UUID == throughUUID {
					return nil
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if throughUUID != "" {
			return errors.New("claude transfer: the pinned fork cursor is missing from the source transcript")
		}
		return nil
	}
	// Metadata sidecars are small JSON objects. Their child identity stays
	// scoped by the copied root; only an explicit root sessionId is rewritten.
	if strings.HasSuffix(source.Path, ".meta.json") {
		if info.Size() > scannerBufMax {
			return errors.New("claude transfer: oversized native metadata")
		}
		data, err := io.ReadAll(io.LimitReader(file, scannerBufMax+1))
		if err != nil {
			return err
		}
		if len(data) > scannerBufMax {
			return errors.New("claude transfer: oversized native metadata")
		}
		data, err = copyTransferRecord(data, fromID, toID)
		if err != nil {
			return err
		}
		return write(data)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			if writeErr := write(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func copyTransferRecord(data []byte, fromID, toID string) ([]byte, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(data, &row); err != nil || row == nil {
		return nil, errors.New("claude transfer: invalid native record")
	}
	var id string
	if value := row["sessionId"]; len(value) > 0 {
		if err := json.Unmarshal(value, &id); err != nil {
			return nil, err
		}
	}
	if id == fromID {
		row["sessionId"], _ = json.Marshal(toID)
	}
	// Compact JSON is significant: Claude's project-wide resume discovery
	// uses compact header probes before parsing a candidate file.
	return json.Marshal(row)
}
