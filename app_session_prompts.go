package main

import "strings"

func (a *App) threadSystemPrompt(threadID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.threadSystemPrompts == nil {
		return ""
	}
	return a.threadSystemPrompts[threadID]
}

func (a *App) setThreadSystemPrompt(threadID, prompt string) {
	threadID = strings.TrimSpace(threadID)
	prompt = strings.TrimSpace(prompt)
	if threadID == "" || prompt == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.threadSystemPrompts == nil {
		a.threadSystemPrompts = make(map[string]string)
	}
	a.threadSystemPrompts[threadID] = prompt
}

func (a *App) clearThreadSystemPrompt(threadID string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.threadSystemPrompts, threadID)
}

func joinSystemPrompts(parts ...string) string {
	joined := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			joined = append(joined, part)
		}
	}
	return strings.Join(joined, "\n\n")
}
