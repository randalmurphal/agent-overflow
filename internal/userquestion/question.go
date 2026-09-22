// Package userquestion defines assistant-authored questions answered by a new user message.
package userquestion

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Question struct {
	Title   string   `json:"title"`
	Options []string `json:"options,omitempty"`
}

type Metadata struct {
	BlockType      string     `json:"blockType,omitempty"`
	Delivery       string     `json:"delivery"`
	ProviderItemID string     `json:"provider_item_id,omitempty"`
	Questions      []Question `json:"questions"`
}

func Decode(raw []byte) (Metadata, bool, error) {
	var probe struct {
		Questions json.RawMessage `json:"questions"`
	}
	if len(raw) == 0 {
		return Metadata{}, false, nil
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Metadata{}, false, err
	}
	if len(probe.Questions) == 0 || string(probe.Questions) == "null" {
		return Metadata{}, false, nil
	}
	var meta Metadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, true, err
	}
	if meta.Delivery != "async" {
		return meta, true, fmt.Errorf("structured async questions require async delivery")
	}
	if err := Validate(meta.Questions); err != nil {
		return meta, true, err
	}
	return meta, true, nil
}

func Validate(questions []Question) error {
	if len(questions) == 0 {
		return fmt.Errorf("questions must not be empty")
	}
	for i, q := range questions {
		if strings.TrimSpace(q.Title) == "" {
			return fmt.Errorf("question %d has an empty title", i+1)
		}
		if q.Options != nil && len(q.Options) == 0 {
			return fmt.Errorf("question %d has empty options", i+1)
		}
		for _, option := range q.Options {
			if strings.TrimSpace(option) == "" {
				return fmt.Errorf("question %d has a blank option", i+1)
			}
		}
	}
	return nil
}

func ItemID(providerID string) string { return "question:" + providerID }

func Summary(questions []Question) string {
	parts := make([]string, len(questions))
	for i, q := range questions {
		parts[i] = q.Title
	}
	return strings.Join(parts, "\n\n")
}
