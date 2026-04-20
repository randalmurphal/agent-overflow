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

func (a *App) SavePayloadToFile(payloadID string) (string, error) {
	if a.app == nil {
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

	path, err := a.app.Dialog.SaveFile().SetFilename(filename).PromptForSingleSelection()
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
	threads, err := a.store.ListThreads()
	if err != nil {
		return store.Item{}, fmt.Errorf("list threads for payload lookup: %w", err)
	}
	for _, thread := range threads {
		items, err := a.store.ListItems(thread.ID)
		if err != nil {
			return store.Item{}, fmt.Errorf("list items for payload lookup in %s: %w", thread.ID, err)
		}
		for _, item := range items {
			if item.PayloadID == payloadID {
				return item, nil
			}
		}
	}
	return store.Item{}, fmt.Errorf("payload %s not linked to any item", payloadID)
}
