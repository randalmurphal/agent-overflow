package compare

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func readLogicalPanes(path string) ([]LogicalPane, error) {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+escaped+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if present, tableErr := tableExists(db, "ui_state"); tableErr != nil {
		return nil, fmt.Errorf("inspect ui_state table: %w", tableErr)
	} else if !present {
		return []LogicalPane{}, nil
	}
	rows, err := db.Query(`SELECT key, value FROM ui_state WHERE key='paneLayout' ORDER BY scope`)
	if err != nil {
		return nil, fmt.Errorf("read pane layout: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		var layout struct {
			Panes []struct {
				PaneID       string  `json:"paneId"`
				Kind         string  `json:"kind"`
				ThreadID     string  `json:"threadId"`
				SourcePaneID string  `json:"sourcePaneId"`
				WidthPX      float64 `json:"widthPx"`
			} `json:"panes"`
		}
		if err := json.Unmarshal([]byte(value), &layout); err != nil {
			return nil, fmt.Errorf("parse pane layout in scope %q: %w", key, err)
		}
		out := make([]LogicalPane, 0, len(layout.Panes))
		for _, p := range layout.Panes {
			if p.PaneID != "" {
				out = append(out, LogicalPane{PaneID: p.PaneID, Kind: p.Kind, ThreadID: p.ThreadID, SourcePaneID: p.SourcePaneID, WidthPX: p.WidthPX})
			}
		}
		return out, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return []LogicalPane{}, nil
}

type eventInput struct {
	Timestamp int64           `json:"ts"`
	ThreadID  string          `json:"threadId"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	file      string
	line      int
}

func collectEvents(root, target string) (EventStream, error) {
	var files []string
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			return EventStream{}, fmt.Errorf("write empty event stream: %w", err)
		}
		return EventStream{Path: "events.jsonl", SHA256: hashBytes(nil), Events: []Event{}}, nil
	} else if err != nil {
		return EventStream{}, fmt.Errorf("inspect replay source: %w", err)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("replay source %s is not a regular file", path)
		}
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return EventStream{}, fmt.Errorf("walk replay source: %w", err)
	}
	sort.Strings(files)
	var records []eventInput
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return EventStream{}, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			raw := bytesTrim(scanner.Bytes())
			if len(raw) == 0 {
				continue
			}
			var rec eventInput
			if err := json.Unmarshal(raw, &rec); err != nil {
				f.Close()
				return EventStream{}, fmt.Errorf("parse replay %s line %d: %w", path, lineNo, err)
			}
			if rec.Kind == "" {
				f.Close()
				return EventStream{}, fmt.Errorf("replay %s line %d has no kind", path, lineNo)
			}
			rec.file, rec.line = path, lineNo
			records = append(records, rec)
		}
		if err := scanner.Err(); err != nil {
			f.Close()
			return EventStream{}, err
		}
		f.Close()
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Timestamp != records[j].Timestamp {
			return records[i].Timestamp < records[j].Timestamp
		}
		if records[i].file != records[j].file {
			return records[i].file < records[j].file
		}
		return records[i].line < records[j].line
	})
	events := make([]Event, 0, len(records))
	var stream []byte
	for i, rec := range records {
		data := append(json.RawMessage(nil), rec.Data...)
		e := Event{Ordinal: i + 1, Timestamp: rec.Timestamp, ThreadID: rec.ThreadID, Kind: rec.Kind, Data: data}
		base, _ := json.Marshal(struct {
			Ordinal   int             `json:"ordinal"`
			Timestamp int64           `json:"ts"`
			ThreadID  string          `json:"threadId"`
			Kind      string          `json:"kind"`
			Data      json.RawMessage `json:"data,omitempty"`
		}{e.Ordinal, e.Timestamp, e.ThreadID, e.Kind, e.Data})
		e.SHA256 = hashBytes(base)
		line, _ := json.Marshal(e)
		stream = append(stream, line...)
		stream = append(stream, '\n')
		events = append(events, e)
	}
	if err := os.WriteFile(target, stream, 0o600); err != nil {
		return EventStream{}, fmt.Errorf("write event stream: %w", err)
	}
	return EventStream{Path: "events.jsonl", Count: len(events), SHA256: hashBytes(stream), Events: events}, nil
}

func bytesTrim(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }
