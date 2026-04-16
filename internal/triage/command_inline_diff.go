package triage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type pendingCommandInlineDiff struct {
	ThreadID string
	Meta     ToolResultMeta
	DiffData []byte
}

type parsedSupportedShellMutationCommand struct {
	NormalizedCommand string
	Operations        []supportedShellMutationOperation
}

type supportedShellMutationOperation struct {
	Kind    string
	Path    string
	OldPath string
	NewPath string
}

type capturedShellMutationOperation struct {
	Kind            string
	Path            string
	OldPath         string
	NewPath         string
	OriginalContent *string
	Exact           bool
}

func (r *Router) capturePendingCommandInlineDiff(evt provider.ProviderEvent) error {
	if evt.ItemType != "command_execution" || evt.ItemID == "" || len(evt.Meta) == 0 {
		return nil
	}

	thread, err := r.store.GetThread(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("lookup thread for command inline diff: %w", err)
	}

	meta, diffData, ok := captureCommandExecutionToolResult(evt.Meta, thread.WorkspacePath)
	if !ok {
		r.clearPendingCommandInlineDiff(evt.ThreadID, evt.ItemID)
		return nil
	}

	r.setPendingCommandInlineDiff(evt.ThreadID, evt.ItemID, pendingCommandInlineDiff{
		ThreadID: evt.ThreadID,
		Meta:     meta,
		DiffData: diffData,
	})
	return nil
}

func (r *Router) persistCommandInlineDiffToolResult(evt provider.ProviderEvent) error {
	if evt.ItemType != "command_execution" || evt.ItemID == "" {
		return nil
	}

	pending, ok := r.takePendingCommandInlineDiff(evt.ThreadID, evt.ItemID)
	if !ok || extractRuntimeCommandExitCode(evt.Meta) != 0 {
		return nil
	}

	return r.persistToolResult(evt, pending.Meta, pending.DiffData)
}

func (r *Router) persistToolResult(evt provider.ProviderEvent, meta ToolResultMeta, diffData []byte) error {
	if evt.ItemID == "" {
		return nil
	}

	now := evt.Timestamp.UnixMilli()
	if now == 0 {
		now = time.Now().UnixMilli()
	}

	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		return fmt.Errorf("tool result turn index: %w", err)
	}

	payloadID := toolResultPayloadID(evt.ItemID)
	meta, diffData = r.mergeToolResultPayload(payloadID, meta, diffData)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal tool result meta: %w", err)
	}

	payload := store.Payload{
		ID:        payloadID,
		Kind:      toolResultPayloadKind,
		Meta:      string(metaJSON),
		Data:      diffData,
		CreatedAt: now,
	}
	if err := r.store.UpsertPayload(payload); err != nil {
		return fmt.Errorf("persist tool result payload: %w", err)
	}
	r.emitPayloadMeta(payloadID, evt.ThreadID, toolResultPayloadKind, string(metaJSON), now)

	item, found, err := r.store.GetItem(evt.ItemID)
	if err != nil {
		return fmt.Errorf("lookup tool result item: %w", err)
	}
	summary := summarizeToolResult(meta)
	if found {
		return r.store.UpdateItemPayload(item.ID, payloadID, summary, now)
	}

	itemIndex, err := r.store.NextItemIndex(evt.ThreadID, turnIndex)
	if err != nil {
		return fmt.Errorf("tool result item index: %w", err)
	}

	return r.store.InsertItem(store.Item{
		ID:        evt.ItemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      toolResultItemKind,
		Role:      "assistant",
		Summary:   summary,
		PayloadID: payloadID,
		CreatedAt: now,
	})
}

func captureCommandExecutionToolResult(raw json.RawMessage, workspaceRoot string) (ToolResultMeta, []byte, bool) {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ToolResultMeta{}, nil, false
	}

	item := asAnyMap(payload["item"])
	if item == nil {
		return ToolResultMeta{}, nil, false
	}

	command := extractRuntimeToolCommand(asAnyMap(item["data"]))
	if command == "" {
		return ToolResultMeta{}, nil, false
	}

	parsed := parseSupportedShellMutationCommand(command, workspaceRoot)
	if parsed == nil || hasDependentShellMutationPaths(parsed.Operations) {
		return ToolResultMeta{}, nil, false
	}

	captured, ok := captureShellMutationOperations(parsed.Operations, workspaceRoot)
	if !ok {
		return ToolResultMeta{}, nil, false
	}

	inlineDiff, unifiedDiff := buildCommandExecutionInlineDiffArtifact(captured)
	if inlineDiff == nil {
		return ToolResultMeta{}, nil, false
	}

	meta := ToolResultMeta{
		ItemType:   "command_execution",
		Title:      firstNonEmpty(asTrimmedString(item["title"]), "Run command"),
		Detail:     parsed.NormalizedCommand,
		InlineDiff: inlineDiff,
	}
	meta.Preview = toolPreview(meta)
	return meta, []byte(unifiedDiff), true
}

func captureShellMutationOperations(
	operations []supportedShellMutationOperation,
	workspaceRoot string,
) ([]capturedShellMutationOperation, bool) {
	captured := make([]capturedShellMutationOperation, 0, len(operations))
	for _, operation := range operations {
		if operation.Kind == "delete" {
			next, ok := captureDeleteOperation(operation, workspaceRoot)
			if !ok {
				return nil, false
			}
			captured = append(captured, next)
			continue
		}

		next, ok := captureRenameOperation(operation, workspaceRoot)
		if !ok {
			return nil, false
		}
		captured = append(captured, next)
	}
	return captured, true
}

func captureDeleteOperation(
	operation supportedShellMutationOperation,
	workspaceRoot string,
) (capturedShellMutationOperation, bool) {
	absolutePath := filepath.Join(workspaceRoot, filepath.FromSlash(operation.Path))
	stat, err := os.Lstat(absolutePath)
	if err == nil && stat.IsDir() {
		return capturedShellMutationOperation{}, false
	}

	result := capturedShellMutationOperation{
		Kind:  "delete",
		Path:  operation.Path,
		Exact: true,
	}
	if err == nil && stat.Mode().IsRegular() {
		content, readErr := os.ReadFile(absolutePath)
		if readErr == nil {
			original := string(content)
			result.OriginalContent = &original
			return result, true
		}
	}
	result.Exact = false
	return result, true
}

func captureRenameOperation(
	operation supportedShellMutationOperation,
	workspaceRoot string,
) (capturedShellMutationOperation, bool) {
	oldPath := filepath.Join(workspaceRoot, filepath.FromSlash(operation.OldPath))
	newPath := filepath.Join(workspaceRoot, filepath.FromSlash(operation.NewPath))

	sourceStat, sourceErr := os.Lstat(oldPath)
	if sourceErr == nil && sourceStat.IsDir() {
		return capturedShellMutationOperation{}, false
	}
	if destinationStat, err := os.Lstat(newPath); err == nil && destinationStat != nil {
		return capturedShellMutationOperation{}, false
	}

	return capturedShellMutationOperation{
		Kind:    "rename",
		OldPath: operation.OldPath,
		NewPath: operation.NewPath,
		Exact:   sourceErr == nil && sourceStat.Mode().IsRegular(),
	}, true
}

func buildCommandExecutionInlineDiffArtifact(
	operations []capturedShellMutationOperation,
) (*ToolInlineDiff, string) {
	if len(operations) == 0 {
		return nil, ""
	}

	files := summarizeCapturedCommandFiles(operations)
	if len(files) == 0 {
		return nil, ""
	}

	fragments := make([]string, 0, len(operations))
	exact := true
	deletions := 0
	for _, operation := range operations {
		switch operation.Kind {
		case "delete":
			if operation.OriginalContent == nil {
				exact = false
				continue
			}
			deletions += len(splitRawFileContentLines(*operation.OriginalContent))
			fragments = append(fragments, buildDeletedFileUnifiedDiff(operation.Path, *operation.OriginalContent))
		case "rename":
			if !operation.Exact {
				exact = false
				continue
			}
			fragments = append(fragments, buildRenamedFileUnifiedDiff(operation.OldPath, operation.NewPath))
		}
	}

	if !exact || len(fragments) != len(operations) {
		return &ToolInlineDiff{
			Availability: "summary_only",
			Files:        files,
		}, ""
	}

	return &ToolInlineDiff{
		Availability: "exact_patch",
		Files:        files,
		Deletions:    deletions,
	}, strings.Join(fragments, "\n\n")
}

func summarizeCapturedCommandFiles(operations []capturedShellMutationOperation) []ToolInlineDiffFile {
	byPath := make(map[string]ToolInlineDiffFile, len(operations))
	for _, operation := range operations {
		if operation.Kind == "delete" {
			file := ToolInlineDiffFile{
				Path: operation.Path,
				Kind: "deleted",
			}
			if operation.OriginalContent != nil {
				file.Deletions = len(splitRawFileContentLines(*operation.OriginalContent))
			}
			byPath[file.Path] = file
			continue
		}

		byPath[operation.NewPath] = ToolInlineDiffFile{
			Path: operation.NewPath,
			Kind: "renamed",
		}
	}

	files := make([]ToolInlineDiffFile, 0, len(byPath))
	for _, file := range byPath {
		files = append(files, file)
	}
	slices.SortFunc(files, func(left, right ToolInlineDiffFile) int {
		return strings.Compare(left.Path, right.Path)
	})
	return files
}

func buildDeletedFileUnifiedDiff(path, rawContent string) string {
	lines := splitRawFileContentLines(rawContent)
	section := []string{
		fmt.Sprintf("diff --git a/%s b/%s", path, path),
		"deleted file mode 100644",
		fmt.Sprintf("--- a/%s", path),
		"+++ /dev/null",
	}
	if len(lines) == 0 {
		return strings.Join(section, "\n")
	}

	section = append(section, fmt.Sprintf("@@ -1,%d +0,0 @@", len(lines)))
	for _, line := range lines {
		section = append(section, "-"+line)
	}
	return strings.Join(section, "\n")
}

func buildRenamedFileUnifiedDiff(oldPath, newPath string) string {
	return strings.Join([]string{
		fmt.Sprintf("diff --git a/%s b/%s", oldPath, newPath),
		fmt.Sprintf("rename from %s", oldPath),
		fmt.Sprintf("rename to %s", newPath),
		fmt.Sprintf("--- a/%s", oldPath),
		fmt.Sprintf("+++ b/%s", newPath),
	}, "\n")
}

func splitRawFileContentLines(content string) []string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if normalized == "" {
		return nil
	}
	lines := strings.Split(normalized, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}
	return lines
}

func parseSupportedShellMutationCommand(
	command string,
	workspaceRoot string,
) *parsedSupportedShellMutationCommand {
	program := unwrapShellProgram(command)
	if program == "" || containsUnsupportedShellSyntax(program) {
		return nil
	}

	statements := splitShellStatements(program)
	if len(statements) == 0 {
		return nil
	}

	operations := make([]supportedShellMutationOperation, 0, len(statements))
	for _, statement := range statements {
		parsed := parseStatementOperations(statement, workspaceRoot)
		if len(parsed) == 0 {
			return nil
		}
		operations = append(operations, parsed...)
	}

	if len(operations) == 0 {
		return nil
	}
	return &parsedSupportedShellMutationCommand{
		NormalizedCommand: normalizeWhitespace(program),
		Operations:        operations,
	}
}

func unwrapShellProgram(command string) string {
	normalized := strings.TrimSpace(command)
	if normalized == "" {
		return ""
	}
	outerTokens := tokenizeShellWords(normalized)
	if len(outerTokens) == 3 && isSupportedShellWrapperBinary(outerTokens[0]) && outerTokens[1] == "-lc" {
		return strings.TrimSpace(outerTokens[2])
	}
	return normalized
}

func parseStatementOperations(statement string, workspaceRoot string) []supportedShellMutationOperation {
	tokens := tokenizeShellWords(statement)
	if len(tokens) == 0 {
		return nil
	}

	first := commandBasename(tokens[0])
	switch first {
	case "rm":
		return parseDeleteOperations(tokens, workspaceRoot)
	case "mv":
		rename, ok := parseRenameOperation(tokens, []string{"f", "n", "v"}, workspaceRoot)
		if !ok {
			return nil
		}
		return []supportedShellMutationOperation{rename}
	case "git":
		if len(tokens) < 2 {
			return nil
		}
		switch strings.ToLower(tokens[1]) {
		case "rm":
			return parseDeleteOperations(append([]string{tokens[1]}, tokens[2:]...), workspaceRoot)
		case "mv":
			rename, ok := parseRenameOperation(append([]string{tokens[1]}, tokens[2:]...), []string{"f", "k", "v"}, workspaceRoot)
			if !ok {
				return nil
			}
			return []supportedShellMutationOperation{rename}
		}
	}

	return nil
}

func parseDeleteOperations(tokens []string, workspaceRoot string) []supportedShellMutationOperation {
	paths := make([]supportedShellMutationOperation, 0, len(tokens))
	consumeFlags := true
	for _, token := range tokens[1:] {
		if consumeFlags && token == "--" {
			consumeFlags = false
			continue
		}
		if consumeFlags && strings.HasPrefix(token, "-") {
			if token == "-r" || token == "-R" || token == "--recursive" || token == "--cached" {
				return nil
			}
			if token == "-f" || shortFlagsAllowed(token, "f") {
				continue
			}
			return nil
		}
		normalizedPath := normalizeRepoRelativePath(token, workspaceRoot)
		if normalizedPath == "" {
			return nil
		}
		paths = append(paths, supportedShellMutationOperation{
			Kind: "delete",
			Path: normalizedPath,
		})
	}
	return paths
}

func parseRenameOperation(
	tokens []string,
	allowedFlags []string,
	workspaceRoot string,
) (supportedShellMutationOperation, bool) {
	args := make([]string, 0, len(tokens))
	consumeFlags := true
	for _, token := range tokens[1:] {
		if consumeFlags && token == "--" {
			consumeFlags = false
			continue
		}
		if consumeFlags && strings.HasPrefix(token, "-") {
			if !shortFlagsAllowed(token, allowedFlags...) {
				return supportedShellMutationOperation{}, false
			}
			continue
		}
		args = append(args, token)
	}

	if len(args) != 2 {
		return supportedShellMutationOperation{}, false
	}

	oldPath := normalizeRepoRelativePath(args[0], workspaceRoot)
	newPath := normalizeRepoRelativePath(args[1], workspaceRoot)
	if oldPath == "" || newPath == "" || oldPath == newPath {
		return supportedShellMutationOperation{}, false
	}
	return supportedShellMutationOperation{
		Kind:    "rename",
		OldPath: oldPath,
		NewPath: newPath,
	}, true
}

func hasDependentShellMutationPaths(operations []supportedShellMutationOperation) bool {
	touched := make(map[string]struct{}, len(operations)*2)
	for _, operation := range operations {
		paths := []string{operation.Path}
		if operation.Kind == "rename" {
			paths = []string{operation.OldPath, operation.NewPath}
		}
		for _, path := range paths {
			if path == "" {
				continue
			}
			if _, ok := touched[path]; ok {
				return true
			}
			touched[path] = struct{}{}
		}
	}
	return false
}

func normalizeRepoRelativePath(filePath, workspaceRoot string) string {
	cleanPath := filepath.Clean(strings.TrimSpace(filePath))
	if cleanPath == "." || cleanPath == "" {
		return ""
	}
	if filepath.IsAbs(cleanPath) {
		rel, err := filepath.Rel(filepath.Clean(workspaceRoot), cleanPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		cleanPath = rel
	}
	return filepath.ToSlash(cleanPath)
}

func normalizeWhitespace(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func containsUnsupportedShellSyntax(program string) bool {
	if strings.TrimSpace(program) == "" {
		return true
	}

	var quote byte
	escaped := false
	for index := 0; index < len(program); index++ {
		current := program[index]
		next := byte(0)
		if index+1 < len(program) {
			next = program[index+1]
		}

		if escaped {
			escaped = false
			continue
		}
		if quote == '\'' {
			if current == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			switch current {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			case '`', '$':
				return true
			}
			continue
		}

		switch current {
		case '\\':
			escaped = true
		case '\'', '"':
			quote = current
		case '`', '|', '>', '<', '(', ')', '$', '*', '?', '[', ']', '{', '}', '~':
			return true
		case '&':
			if next != '&' {
				return true
			}
			index++
		}
	}
	return escaped || quote != 0
}

func splitShellStatements(program string) []string {
	var parts []string
	var current strings.Builder
	var quote rune
	escaped := false

	for i := 0; i < len(program); i++ {
		char := rune(program[i])
		next := byte(0)
		if i+1 < len(program) {
			next = program[i+1]
		}

		if escaped {
			current.WriteByte(program[i])
			escaped = false
			continue
		}
		if char == '\\' {
			current.WriteByte(program[i])
			escaped = true
			continue
		}
		if quote != 0 {
			current.WriteByte(program[i])
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			current.WriteByte(program[i])
			quote = char
			continue
		}
		if char == ';' || (char == '&' && next == '&') {
			statement := strings.TrimSpace(current.String())
			if statement == "" {
				return nil
			}
			parts = append(parts, statement)
			current.Reset()
			if char == '&' {
				i++
			}
			continue
		}
		current.WriteByte(program[i])
	}

	if escaped || quote != 0 {
		return nil
	}
	tail := strings.TrimSpace(current.String())
	if tail == "" {
		return parts
	}
	return append(parts, tail)
}

func tokenizeShellWords(command string) []string {
	var tokens []string
	var current strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for i := 0; i < len(command); i++ {
		char := rune(command[i])
		if escaped {
			current.WriteByte(command[i])
			escaped = false
			continue
		}

		switch quote {
		case '\'':
			if char == '\'' {
				quote = 0
			} else {
				current.WriteByte(command[i])
			}
			continue
		case '"':
			if char == '"' {
				quote = 0
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			current.WriteByte(command[i])
			continue
		}

		switch {
		case char == '\\':
			escaped = true
		case char == '\'' || char == '"':
			quote = char
		case strings.ContainsRune("*?[]{}~", char):
			return nil
		case char == ' ' || char == '\t' || char == '\n':
			flush()
		default:
			current.WriteByte(command[i])
		}
	}

	if escaped || quote != 0 {
		return nil
	}
	flush()
	return tokens
}

func commandBasename(command string) string {
	normalized := strings.ReplaceAll(command, "\\", "/")
	base := filepath.Base(normalized)
	return strings.ToLower(strings.TrimSuffix(base, ".exe"))
}

func isSupportedShellWrapperBinary(command string) bool {
	switch commandBasename(command) {
	case "sh", "bash", "zsh":
		return true
	default:
		return false
	}
}

func shortFlagsAllowed(token string, allowed ...string) bool {
	if !strings.HasPrefix(token, "-") || strings.HasPrefix(token, "--") || len(token) <= 1 {
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, flag := range allowed {
		allowedSet[flag] = struct{}{}
	}
	for _, flag := range strings.Split(token[1:], "") {
		if _, ok := allowedSet[flag]; !ok {
			return false
		}
	}
	return true
}

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

func (r *Router) setPendingCommandInlineDiff(threadID, itemID string, pending pendingCommandInlineDiff) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingCommandDiffs[pendingCommandInlineDiffKey(threadID, itemID)] = pending
}

func (r *Router) takePendingCommandInlineDiff(threadID, itemID string) (pendingCommandInlineDiff, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := pendingCommandInlineDiffKey(threadID, itemID)
	pending, ok := r.pendingCommandDiffs[key]
	delete(r.pendingCommandDiffs, key)
	return pending, ok
}

func (r *Router) clearPendingCommandInlineDiff(threadID, itemID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pendingCommandDiffs, pendingCommandInlineDiffKey(threadID, itemID))
}

func pendingCommandInlineDiffKey(threadID, itemID string) string {
	return threadID + ":" + itemID
}
