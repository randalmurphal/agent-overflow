package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cdplog "github.com/chromedp/cdproto/log"
	cdpruntime "github.com/chromedp/cdproto/runtime"
)

func (p *managedPage) captureConsole(event *cdpruntime.EventConsoleAPICalled) {
	parts := make([]string, 0, len(event.Args))
	for _, arg := range event.Args {
		if arg == nil {
			continue
		}
		var value any
		if len(arg.Value) > 0 && json.Unmarshal(arg.Value, &value) == nil {
			parts = append(parts, fmt.Sprint(value))
		} else if arg.Description != "" {
			parts = append(parts, arg.Description)
		} else {
			parts = append(parts, string(arg.Type))
		}
	}
	level := strings.ToLower(string(event.Type))
	if level == "warning" {
		level = "warn"
	}
	if level != "debug" && level != "info" && level != "warn" && level != "error" {
		level = "log"
	}
	timestamp := time.Now().UTC()
	if event.Timestamp != nil {
		timestamp = time.Time(*event.Timestamp).UTC()
	}
	p.appendLog(ConsoleLog{Level: level, Message: strings.Join(parts, " "), Timestamp: timestamp.Format(time.RFC3339Nano), URL: p.cachedInfo().URL})
}

func (p *managedPage) captureLogEntry(event *cdplog.EventEntryAdded) {
	if event.Entry == nil {
		return
	}
	level := strings.ToLower(string(event.Entry.Level))
	if level == "warning" {
		level = "warn"
	}
	if level != "debug" && level != "info" && level != "warn" && level != "error" {
		level = "log"
	}
	timestamp := time.Now().UTC()
	if event.Entry.Timestamp != nil {
		timestamp = time.Time(*event.Entry.Timestamp).UTC()
	}
	p.appendLog(ConsoleLog{Level: level, Message: event.Entry.Text, Timestamp: timestamp.Format(time.RFC3339Nano), URL: event.Entry.URL})
}

func (p *managedPage) appendLog(entry ConsoleLog) {
	if len(entry.Message) > maxConsoleMessageBytes {
		entry.Message = entry.Message[:maxConsoleMessageBytes]
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if len(p.logs) == maxConsoleEntries {
		copy(p.logs, p.logs[1:])
		p.logs[len(p.logs)-1] = entry
	} else {
		p.logs = append(p.logs, entry)
	}
}

func (m *Manager) ConsoleLogs(_ context.Context, access Access, opts ConsoleOptions) ([]ConsoleLog, error) {
	p, _, err := m.lookupOrSelectPage(context.Background(), access, opts.PageID)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > maxConsoleEntries {
		return nil, fmt.Errorf("browser: console log limit must be between 1 and %d", maxConsoleEntries)
	}
	levels := make(map[string]bool, len(opts.Levels))
	for _, level := range opts.Levels {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == "warning" {
			level = "warn"
		}
		if level != "debug" && level != "info" && level != "log" && level != "warn" && level != "error" {
			return nil, fmt.Errorf("browser: invalid console level %q", level)
		}
		levels[level] = true
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	out := make([]ConsoleLog, 0, limit)
	for i := len(p.logs) - 1; i >= 0 && len(out) < limit; i-- {
		entry := p.logs[i]
		if len(levels) > 0 && !levels[entry.Level] {
			continue
		}
		if opts.Filter != "" && !strings.Contains(strings.ToLower(entry.Message), strings.ToLower(opts.Filter)) {
			continue
		}
		out = append(out, entry)
	}
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out, nil
}
