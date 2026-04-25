package main

import (
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/store"
)

type PayloadPreview struct {
	Data       string `json:"data"`
	NextOffset int    `json:"nextOffset"`
	TotalSize  int    `json:"totalSize"`
	IsComplete bool   `json:"isComplete"`
}

type PayloadChunk struct {
	Data       string `json:"data"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"nextOffset"`
	TotalSize  int    `json:"totalSize"`
	IsComplete bool   `json:"isComplete"`
}

// PayloadContent returns a payload's raw bytes. Rendering is a frontend
// projection based on the payload kind.
type PayloadContent struct {
	Data string `json:"data"`
}

func (a *App) GetPayloadPreview(threadID string, payloadID string, maxBytes int) (PayloadPreview, error) {
	if err := a.flushThreadPayloadBuffers(threadID); err != nil {
		return PayloadPreview{}, err
	}
	if _, err := a.getThreadPayloadMeta(threadID, payloadID); err != nil {
		return PayloadPreview{}, err
	}
	data, totalSize, isComplete, err := a.store.GetPayloadPreview(payloadID, maxBytes)
	if err != nil {
		return PayloadPreview{}, err
	}
	return PayloadPreview{
		Data:       string(data),
		NextOffset: len(data),
		TotalSize:  totalSize,
		IsComplete: isComplete,
	}, nil
}

func (a *App) GetPayloadChunk(threadID string, payloadID string, offset int, maxBytes int) (PayloadChunk, error) {
	if err := a.flushThreadPayloadBuffers(threadID); err != nil {
		return PayloadChunk{}, err
	}
	if _, err := a.getThreadPayloadMeta(threadID, payloadID); err != nil {
		return PayloadChunk{}, err
	}
	data, totalSize, isComplete, err := a.store.GetPayloadChunk(payloadID, offset, maxBytes)
	if err != nil {
		return PayloadChunk{}, err
	}
	return PayloadChunk{
		Data:       string(data),
		Offset:     offset,
		NextOffset: offset + len(data),
		TotalSize:  totalSize,
		IsComplete: isComplete,
	}, nil
}

// GetPayloadData returns a payload body. The caller must supply the owning
// thread so payload ids cannot be read outside the thread timeline that
// references them.
func (a *App) GetPayloadData(threadID string, payloadID string) (PayloadContent, error) {
	if err := a.flushThreadPayloadBuffers(threadID); err != nil {
		return PayloadContent{}, err
	}
	if _, err := a.getThreadPayloadMeta(threadID, payloadID); err != nil {
		return PayloadContent{}, err
	}
	data, err := a.store.GetPayloadData(payloadID)
	if err != nil {
		return PayloadContent{}, err
	}
	return PayloadContent{
		Data: string(data),
	}, nil
}

// savePayloadPicker resolves a destination path for SavePayloadToFile.
// Production uses the Wails save dialog; tests inject a stub via
// a.savePayloadPickerFn so the write path is exercisable without a
// running GUI.
type savePayloadPicker func(filename string) (string, error)

func (a *App) resolveSavePayloadPicker() savePayloadPicker {
	if a.savePayloadPickerFn != nil {
		return a.savePayloadPickerFn
	}
	// Headless mode (Phase D's WSL backend) doesn't construct a Wails
	// app — the native save dialog is replaced by a frontend-side
	// download path. Returning nil makes SavePayloadToFile error out
	// with a clear "requires active application" message; the WSL
	// frontend never invokes this path because its UI uses the
	// download fallback.
	if a.app == nil {
		return nil
	}
	return func(filename string) (string, error) {
		return a.app.Dialog.SaveFile().SetFilename(filename).PromptForSingleSelection()
	}
}

func (a *App) SavePayloadToFile(threadID string, payloadID string) (string, error) {
	if err := a.flushThreadPayloadBuffers(threadID); err != nil {
		return "", err
	}
	picker := a.resolveSavePayloadPicker()
	if picker == nil {
		return "", fmt.Errorf("app: save payload to file requires active application")
	}

	item, err := a.findThreadItemByPayloadID(threadID, payloadID)
	if err != nil {
		return "", err
	}

	filename := "payload.txt"
	if item.ToolName != "" {
		filename = item.ToolName + ".txt"
	} else if item.PayloadKind != "" {
		filename = item.PayloadKind + ".txt"
	}
	filename = filepath.Base(filename)

	path, err := picker(filename)
	if err != nil {
		return "", fmt.Errorf("save payload dialog: %w", err)
	}
	if path == "" {
		return "", nil
	}

	data, err := a.store.GetPayloadData(payloadID)
	if err != nil {
		return "", err
	}
	// 0600 — saved payloads can carry tool output and provider responses
	// the user wouldn't want world-readable on a shared host. The user
	// picked the path explicitly, so it's deliberate persistence; lock
	// the file mode down rather than leave it world-readable by default.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write payload to %s: %w", path, err)
	}
	return path, nil
}

func (a *App) flushThreadPayloadBuffers(threadID string) error {
	if a.triage == nil {
		return nil
	}
	if err := a.triage.FlushThread(threadID); err != nil {
		return fmt.Errorf("flush live payload buffers for thread %s: %w", threadID, err)
	}
	return nil
}

func (a *App) getThreadPayloadMeta(threadID string, payloadID string) (store.PayloadMeta, error) {
	if threadID == "" {
		return store.PayloadMeta{}, fmt.Errorf("thread id is required")
	}
	if payloadID == "" {
		return store.PayloadMeta{}, fmt.Errorf("payload id is required")
	}
	if _, err := a.findThreadItemByPayloadID(threadID, payloadID); err != nil {
		return store.PayloadMeta{}, err
	}
	meta, err := a.store.GetPayloadMeta(payloadID)
	if err != nil {
		return store.PayloadMeta{}, err
	}
	return meta, nil
}

func (a *App) findThreadItemByPayloadID(threadID string, payloadID string) (store.Item, error) {
	item, found, err := a.store.GetThreadItemByPayloadID(threadID, payloadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("find item by payload id %s on thread %s: %w", payloadID, threadID, err)
	}
	if !found {
		return store.Item{}, fmt.Errorf("payload %s not linked to thread %s", payloadID, threadID)
	}
	return item, nil
}
