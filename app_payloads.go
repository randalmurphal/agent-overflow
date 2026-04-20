package main

import (
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/store"
)

type PayloadPreview struct {
	Data       string `json:"data"`
	TotalSize  int    `json:"totalSize"`
	IsComplete bool   `json:"isComplete"`
}

func (a *App) GetPayloadPreview(payloadID string, maxBytes int) (PayloadPreview, error) {
	data, totalSize, isComplete, err := a.store.GetPayloadPreview(payloadID, maxBytes)
	if err != nil {
		return PayloadPreview{}, err
	}
	return PayloadPreview{
		Data:       string(data),
		TotalSize:  totalSize,
		IsComplete: isComplete,
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
	if a.app == nil {
		return nil
	}
	return func(filename string) (string, error) {
		return a.app.Dialog.SaveFile().SetFilename(filename).PromptForSingleSelection()
	}
}

func (a *App) SavePayloadToFile(payloadID string) (string, error) {
	picker := a.resolveSavePayloadPicker()
	if picker == nil {
		return "", fmt.Errorf("app: save payload to file requires active application")
	}

	item, err := a.findItemByPayloadID(payloadID)
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
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("write payload to %s: %w", path, err)
	}
	return path, nil
}

func (a *App) findItemByPayloadID(payloadID string) (store.Item, error) {
	item, found, err := a.store.GetItemByPayloadID(payloadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("find item by payload id: %w", err)
	}
	if !found {
		return store.Item{}, fmt.Errorf("payload %s not linked to any item", payloadID)
	}
	return item, nil
}
