package triage

import (
	"path/filepath"
	"strings"
)

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

	switch first := commandBasename(tokens[0]); first {
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
