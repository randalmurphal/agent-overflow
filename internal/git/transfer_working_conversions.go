package git

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/transferfiles"
)

var transferConversionAttributes = []string{"filter", "text", "eol", "working-tree-encoding", "ident"}

// Git's clean comparison deliberately hides smudged LFS contents, line ending
// conversions and encodings. Transfer actual working bytes whenever they differ
// from the raw index blob, so destination filters never have to run. Ordinary
// files without conversion attributes pay no content read here.
func (c *Core) transferConvertedWorkingPaths(ctx context.Context, cwd string, index []TransferIndexEntry, known []string) ([]string, error) {
	if len(index) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, entry := range index {
		input.WriteString(entry.Path)
		input.WriteByte(0)
	}
	attributes := &transferAttributeWriter{index: index, converted: make([]bool, len(index))}
	args := append([]string{"check-attr", "-z", "--stdin"}, transferConversionAttributes...)
	if _, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, stdin: input.String(), args: args,
		output: attributes, outputLimit: maxTransferIndexBytes * 8, timeout: 2 * time.Minute}); err != nil {
		return nil, err
	}
	if attributes.entry != len(index) || attributes.field != 0 || len(attributes.partial) != 0 {
		return nil, errors.New("transfer: incomplete Git attribute response")
	}
	config, err := c.runSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, args: []string{"config", "--get", "core.autocrlf"}})
	if err != nil {
		return nil, err
	}
	if config.exitCode != 0 && config.exitCode != 1 {
		return nil, errors.New("transfer: cannot inspect working line ending policy")
	}
	crlf := strings.ToLower(strings.TrimSpace(config.stdout))
	convertAll := crlf != "" && crlf != "false"
	seen := make(map[string]bool, len(known))
	for _, path := range known {
		seen[path] = true
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	buffer := make([]byte, 128<<10)
	var changed []string
	for i, entry := range index {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seen[entry.Path] || entry.Mode == "120000" || (!convertAll && !attributes.converted[i]) {
			continue
		}
		same, err := transferWorkingMatchesBlob(ctx, root, entry, buffer)
		if err != nil {
			return nil, err
		}
		if !same {
			changed = append(changed, entry.Path)
		}
	}
	return changed, nil
}

func transferWorkingMatchesBlob(ctx context.Context, root *os.Root, entry TransferIndexEntry, buffer []byte) (bool, error) {
	// The same path walk used by capture refuses traversal through old links.
	working, before, err := readWorkingEntry(root, entry.Path)
	if err != nil {
		return false, err
	}
	if working.Kind != "file" {
		return false, nil
	}
	if before.Size() > transferfiles.MaxFileBytes {
		return false, errors.New("The converted working file exceeds the transfer size limit.")
	}
	file, err := root.Open(filepath.FromSlash(entry.Path))
	if err != nil {
		return false, err
	}
	defer file.Close()
	var digest hash.Hash = sha1.New()
	if len(entry.OID) == 64 {
		digest = sha256.New()
	}
	fmt.Fprintf(digest, "blob %d\x00", before.Size())
	remaining := before.Size()
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := io.ReadFull(file, buffer[:min(int64(len(buffer)), remaining)])
		if err != nil {
			return false, fmt.Errorf("transfer: converted working file changed: %w", err)
		}
		_, _ = digest.Write(buffer[:n])
		remaining -= int64(n)
	}
	after, err := file.Stat()
	if err != nil {
		return false, err
	}
	current, err := root.Lstat(filepath.FromSlash(entry.Path))
	if err != nil {
		return false, err
	}
	if !sameTransferWorkingFile(before, after) || !sameTransferWorkingFile(before, current) {
		return false, errors.New("A converted working file changed while preparing the transfer. Retry after saving it.")
	}
	return fmt.Sprintf("%x", digest.Sum(nil)) == entry.OID, nil
}

func sameTransferWorkingFile(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// check-attr emits five NUL-separated triples per path. Stream them instead of
// retaining repeated paths/attribute values for a large repository in memory.
type transferAttributeWriter struct {
	index                   []TransferIndexEntry
	converted               []bool
	entry, attribute, field int
	partial                 []byte
}

func (w *transferAttributeWriter) Write(data []byte) (int, error) {
	consumed := 0
	for len(data) > 0 {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			end = len(data)
		}
		if len(w.partial)+end > 4096 {
			return consumed, errors.New("transfer: oversized Git attribute field")
		}
		w.partial = append(w.partial, data[:end]...)
		data = data[end:]
		consumed += end
		if len(data) == 0 {
			continue
		}
		data = data[1:]
		consumed++
		value := string(w.partial)
		w.partial = w.partial[:0]
		if w.entry >= len(w.index) {
			return consumed, errors.New("transfer: unexpected Git attribute response")
		}
		switch w.field {
		case 0:
			if value != w.index[w.entry].Path {
				return consumed, errors.New("transfer: Git attributes name a different file")
			}
		case 1:
			if value != transferConversionAttributes[w.attribute] {
				return consumed, errors.New("transfer: unexpected Git conversion attribute")
			}
		case 2:
			if value != "" && value != "unspecified" && value != "unset" {
				w.converted[w.entry] = true
			}
		}
		w.field++
		if w.field == 3 {
			w.field = 0
			w.attribute++
			if w.attribute == len(transferConversionAttributes) {
				w.attribute = 0
				w.entry++
			}
		}
	}
	return consumed, nil
}
