package sessionimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// convertSubagentTranscriptFile projects a terminal sidechain without
// retaining its JSONL or decoded rows. The first pass seeds the clock and
// pairs compaction summaries exactly as ConvertSubagentRows does; the second
// converts one row at a time. Both passes read the same file-size snapshot,
// so a report appended after task_notification cannot change the pairing
// between them.
func convertSubagentTranscriptFile(path, scope string) (result ConvertResult, err error) {
	file, err := os.Open(path)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("open subagent transcript %q: %w", path, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close subagent transcript %q: %w", path, closeErr))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return ConvertResult{}, fmt.Errorf("stat subagent transcript %q: %w", path, err)
	}
	seed, summaries, consumed, err := indexSubagentFile(file, info.Size())
	if err != nil {
		return ConvertResult{}, fmt.Errorf("index subagent transcript %q: %w", path, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ConvertResult{}, fmt.Errorf("rewind subagent transcript %q: %w", path, err)
	}
	c := newSubagentConverter(scope)
	c.lastTimestamp = seed
	c.compactSummaries = summaries
	c.consumedSummary = consumed
	err = eachSubagentFileRow(file, info.Size(), func(row Row) error {
		c.convertRow(row)
		return nil
	})
	if err != nil {
		return ConvertResult{}, fmt.Errorf("project subagent transcript %q: %w", path, err)
	}
	c.appendDeferredWarnings()
	return ConvertResult{Events: c.events, Warnings: c.warnings}, nil
}

type subagentCompactSummary struct {
	uuid string
	text string
}

func indexSubagentFile(file *os.File, size int64) (int64, map[string]string, map[string]bool, error) {
	var seed int64
	boundaries := make(map[string]bool)
	summaryByParent := make(map[string]subagentCompactSummary)
	err := eachSubagentFileLine(file, size, func(line []byte) error {
		row, ok := decodeSkeletonRow(line, 0, 0)
		if !ok || !admitsTranscriptRow(row) || row.Type == "progress" {
			return nil
		}
		if seed == 0 && row.Timestamp > 0 {
			seed = row.Timestamp
		}
		if row.Type == "system" && row.Subtype == "compact_boundary" {
			boundaries[row.UUID] = true
		}
		if !row.IsCompactSummary || row.ParentUUID == "" {
			return nil
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			return fmt.Errorf("decode compact summary %s: %w", row.UUID, err)
		}
		row.Raw = raw
		text, isString := contentString(messageOf(row))
		if !isString {
			text = blockText(contentBlocks(messageOf(row)))
		}
		summaryByParent[row.ParentUUID] = subagentCompactSummary{uuid: row.UUID, text: text}
		return nil
	})
	if err != nil {
		return 0, nil, nil, err
	}
	summaries := make(map[string]string, len(summaryByParent))
	consumed := make(map[string]bool, len(summaryByParent))
	for parent, summary := range summaryByParent {
		if !boundaries[parent] {
			continue
		}
		summaries[parent] = summary.text
		consumed[summary.uuid] = true
	}
	return seed, summaries, consumed, nil
}

// A sidechain file is trusted provider history, not a UI payload. There is
// no whole-file or per-line display ceiling: each line is released after
// projection, and a truncated final JSONL line is skipped like session
// import. A file shortened during either pass is an error, not success with
// a silently incomplete transcript.
func eachSubagentFileLine(reader io.Reader, size int64, visit func([]byte) error) error {
	limited := &io.LimitedReader{R: reader, N: size}
	if err := eachSubagentLine(limited, visit); err != nil {
		return err
	}
	if limited.N != 0 {
		return fmt.Errorf("subagent transcript shortened by %d bytes during read", limited.N)
	}
	return nil
}

func eachSubagentFileRow(reader io.Reader, size int64, visit func(Row) error) error {
	limited := &io.LimitedReader{R: reader, N: size}
	if err := eachSubagentRow(limited, visit); err != nil {
		return err
	}
	if limited.N != 0 {
		return fmt.Errorf("subagent transcript shortened by %d bytes during read", limited.N)
	}
	return nil
}

func eachSubagentRow(reader io.Reader, visit func(Row) error) error {
	index := 0
	return eachSubagentLine(reader, func(line []byte) error {
		row, ok := decodeSkeletonRow(line, index, 0)
		if !ok || !admitsTranscriptRow(row) {
			return nil
		}
		index++
		if row.Type == "progress" {
			return nil
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			return fmt.Errorf("decode subagent row %s: %w", row.UUID, err)
		}
		return visit(newRow(raw, index-1))
	})
}

func eachSubagentLine(reader io.Reader, visit func([]byte) error) error {
	scanner := newSubagentTranscriptScanner(reader)
	for {
		line, readErr := scanner.next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if err := visit(line.Data); err != nil {
			return err
		}
	}
	return nil
}
