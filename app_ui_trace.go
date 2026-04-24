package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	uiTraceDirName       = "ui-trace"
	uiTraceFileName      = "ui-render.jsonl"
	uiTraceMaxBatchLines = 1000
	uiTraceMaxBatchBytes = 2 * 1024 * 1024
	uiTraceMaxLineBytes  = 64 * 1024
	uiTraceMaxFileBytes  = 10 * 1024 * 1024
)

// GetUIRenderTracePath returns the dev trace file path used by
// AppendUIRenderTraceBatch. The frontend exposes it through the console trace
// API so a debug run can be inspected after a visual glitch.
func (a *App) GetUIRenderTracePath() (string, error) {
	if a.configDir == "" {
		return "", errors.New("ui trace path unavailable before app data directory is initialized")
	}
	return filepath.Join(a.configDir, uiTraceDirName, uiTraceFileName), nil
}

// AppendUIRenderTraceBatch appends compact dev-only UI render trace records.
// The frontend batches calls so rendering never waits on disk. This binding
// still validates each line because it writes directly into the user's config
// directory.
func (a *App) AppendUIRenderTraceBatch(lines []string) (string, error) {
	if len(lines) == 0 {
		return a.GetUIRenderTracePath()
	}
	if len(lines) > uiTraceMaxBatchLines {
		return "", fmt.Errorf("ui trace batch has %d lines, max %d", len(lines), uiTraceMaxBatchLines)
	}

	path, err := a.GetUIRenderTracePath()
	if err != nil {
		return "", err
	}

	cleaned, byteCount, err := validateUITraceLines(lines)
	if err != nil {
		return "", err
	}
	if len(cleaned) == 0 {
		return path, nil
	}

	a.uiTraceMu.Lock()
	defer a.uiTraceMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", fmt.Errorf("create ui trace directory: %w", err)
	}
	if err := rotateUITraceFileIfNeeded(path, int64(byteCount)); err != nil {
		return "", err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", fmt.Errorf("open ui trace file: %w", err)
	}
	defer file.Close()

	for _, line := range cleaned {
		if _, err := file.WriteString(line + "\n"); err != nil {
			return "", fmt.Errorf("write ui trace line: %w", err)
		}
	}
	return path, nil
}

func validateUITraceLines(lines []string) ([]string, int, error) {
	cleaned := make([]string, 0, len(lines))
	byteCount := 0
	for i, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > uiTraceMaxLineBytes {
			return nil, 0, fmt.Errorf("ui trace line %d is %d bytes, max %d", i, len(line), uiTraceMaxLineBytes)
		}
		if !json.Valid([]byte(line)) {
			return nil, 0, fmt.Errorf("ui trace line %d is not valid JSON", i)
		}
		cleaned = append(cleaned, line)
		byteCount += len(line) + 1
		if byteCount > uiTraceMaxBatchBytes {
			return nil, 0, fmt.Errorf("ui trace batch is %d bytes, max %d", byteCount, uiTraceMaxBatchBytes)
		}
	}
	return cleaned, byteCount, nil
}

func rotateUITraceFileIfNeeded(path string, pendingBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat ui trace file: %w", err)
	}
	if info.Size()+pendingBytes <= uiTraceMaxFileBytes {
		return nil
	}

	rotatedPath := path + ".1"
	if err := os.Remove(rotatedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove previous ui trace rotation: %w", err)
	}
	if err := os.Rename(path, rotatedPath); err != nil {
		return fmt.Errorf("rotate ui trace file: %w", err)
	}
	return nil
}
