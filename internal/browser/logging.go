package browser

import (
	"context"
	"fmt"
	"strings"
)

// normalizeConsoleLevel folds an engine's level vocabulary onto the closed set
// the tool reports and filters on.
func normalizeConsoleLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "warning" {
		level = "warn"
	}
	if level != "debug" && level != "info" && level != "warn" && level != "error" {
		level = "log"
	}
	return level
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
