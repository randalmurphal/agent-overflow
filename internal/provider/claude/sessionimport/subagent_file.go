package sessionimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// A sidechain file is trusted provider history, not a UI payload. There is
// no whole-file or per-line display ceiling: each line is released once
// decoded, and a truncated final JSONL line is skipped like session
// import. A file shortened during the read is an error, not success with a
// silently incomplete transcript.
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
