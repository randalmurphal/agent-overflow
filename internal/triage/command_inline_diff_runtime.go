package triage

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

func extractRuntimeToolCommand(data map[string]any) string {
	item := asAnyMap(data["item"])
	itemResult := asAnyMap(item["result"])
	itemInput := asAnyMap(item["input"])
	candidates := []string{
		normalizeCommandValue(item["command"]),
		normalizeCommandValue(itemInput["command"]),
		normalizeCommandValue(itemResult["command"]),
		normalizeCommandValue(data["command"]),
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func extractRuntimeCommandExitCode(raw json.RawMessage) int {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return -1
	}

	data := asAnyMap(payload["item"])
	itemData := asAnyMap(data["data"])
	item := asAnyMap(itemData["item"])
	itemResult := asAnyMap(item["result"])
	candidates := []any{
		item["exitCode"],
		itemData["exitCode"],
		itemResult["exitCode"],
		itemResult["exit_code"],
	}
	for _, candidate := range candidates {
		switch value := candidate.(type) {
		case float64:
			if value == float64(int(value)) {
				return int(value)
			}
		case int:
			return value
		}
	}
	return -1
}

func normalizeCommandValue(value any) string {
	if text := asTrimmedString(value); text != "" {
		return text
	}

	list, ok := value.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, entry := range list {
		text := asTrimmedString(entry)
		if text != "" {
			parts = append(parts, quoteShellArgument(text))
		}
	}
	return strings.Join(parts, " ")
}

func quoteShellArgument(value string) string {
	if value != "" && isSafeShellArgument(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func isSafeShellArgument(value string) bool {
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		if strings.ContainsRune("_./:@%+=,-", char) {
			continue
		}
		return false
	}
	return true
}

func asAnyMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func asTrimmedString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func normalizeWhitespace(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func commandBasename(command string) string {
	normalized := strings.ReplaceAll(command, "\\", "/")
	base := filepath.Base(normalized)
	return strings.ToLower(strings.TrimSuffix(base, ".exe"))
}
