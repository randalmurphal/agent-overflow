package threadtitleapp

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
	"agent-overflow/internal/usermessage"
)

// contextItemLimit bounds the rows loaded before threadtitle applies its text
// budget. The store reports whether this window dropped matching rows so the
// formatter can preserve the truncation marker.
const contextItemLimit = 200

// Store is the persistence surface title coordination needs.
type Store interface {
	GetThread(threadID string) (store.Thread, error)
	ThreadTitleContextItems(threadID string, limit int) ([]store.Item, bool, error)
	UpdateTitleIfCurrent(threadID, expectedTitle, title string) (bool, error)
}

// Attachments resolves an attachment row to its managed file. Implementations
// must enforce their own storage-root safety; Service additionally verifies
// thread ownership and that the resolved path is a regular file.
type Attachments interface {
	Get(attachmentID string) (store.Attachment, string, bool, error)
}

// Generator is the visibly injected provider boundary. The service owns prompt
// construction and attachment selection; root owns workspace/settings routing
// and the actual provider CLI process.
type Generator func(thread store.Thread, prompt string, imagePaths []string) (string, error)

type Applied struct {
	ThreadID string
	Title    string
}

type Completion struct {
	ThreadID string
	Error    string
}

type Config struct {
	Store       Store
	Attachments Attachments
	Generate    Generator
	Applied     func(Applied)
	Completed   func(Completion)
	Logf        func(format string, args ...any)
}

// Service owns the one in-flight generation slot per thread and all policy
// shared by first-turn generation, later-send healing, and user regeneration.
type Service struct {
	config Config

	mu     sync.Mutex
	active map[string]struct{}
}

func New(config Config) *Service {
	if config.Applied == nil {
		config.Applied = func(Applied) {}
	}
	if config.Completed == nil {
		config.Completed = func(Completion) {}
	}
	if config.Logf == nil {
		config.Logf = log.Printf
	}
	return &Service{config: config}
}

// Auto starts a title generation for a still-default thread. hasPriorItems
// selects regeneration-style context for a later-send heal; when that context
// is unavailable or empty, the current message remains a useful fallback.
func (s *Service) Auto(thread store.Thread, content string, attachments []store.Attachment, hasPriorItems bool) {
	// A user-selected "New Thread" is indistinguishable from the creation
	// sentinel until persistence carries a user-named bit. Preserve the existing
	// behavior: the value of healing failed first-turn generations wins.
	if strings.TrimSpace(thread.Title) != threadtitle.Default || strings.TrimSpace(content) == "" {
		return
	}
	if !s.claim(thread.ID) {
		return
	}

	go s.runClaimed(thread.ID, func() error {
		title, err := s.autoTitle(thread, content, attachments, hasPriorItems)
		if err != nil {
			s.config.Logf("send message: generate thread title: %s", textgen.RedactError(err))
			return err
		}
		if title == "" || title == threadtitle.Default {
			return nil
		}
		if _, err := s.applyIfCurrent(thread.ID, thread.Title, title); err != nil {
			s.config.Logf("send message: apply generated thread title: %v", err)
			return err
		}
		return nil
	})
}

// Regenerate starts a conversation-aware user regeneration and acknowledges
// once the thread exists and the run is either started or joined.
func (s *Service) Regenerate(threadID string) error {
	if s == nil || s.config.Store == nil {
		return fmt.Errorf("regenerate thread title: store unavailable")
	}
	thread, err := s.config.Store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("regenerate thread title: %w", err)
	}
	if !s.claim(threadID) {
		return nil
	}
	go s.runClaimed(threadID, func() error {
		return s.runRegeneration(thread)
	})
	return nil
}

func (s *Service) claim(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.active[threadID]; held {
		return false
	}
	if s.active == nil {
		s.active = make(map[string]struct{})
	}
	s.active[threadID] = struct{}{}
	return true
}

func (s *Service) release(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, threadID)
}

func (s *Service) runClaimed(threadID string, body func() error) {
	var runErr error
	defer func() {
		message := ""
		if runErr != nil {
			message = textgen.RedactError(runErr)
		}
		s.config.Completed(Completion{ThreadID: threadID, Error: message})
	}()
	// Release before completion. A caller entering between the two gets a new
	// run and its own completion instead of joining one already reporting done.
	defer s.release(threadID)
	runErr = body()
}

func (s *Service) autoTitle(thread store.Thread, content string, attachments []store.Attachment, hasPriorItems bool) (string, error) {
	if hasPriorItems {
		threadContext, err := s.threadContext(thread.ID)
		if err != nil {
			s.config.Logf("generate thread title: build context for thread %s: %v", thread.ID, err)
		}
		if threadContext != "" {
			return s.generate(thread, threadtitle.BuildRegeneratePrompt(strings.TrimSpace(thread.Title), threadContext), nil)
		}
	}

	imagePaths, err := s.imagePaths(thread.ID, attachments)
	if err != nil {
		return "", err
	}
	return s.generate(thread, threadtitle.BuildPrompt(content, attachments), imagePaths)
}

func (s *Service) runRegeneration(thread store.Thread) error {
	previousTitle := strings.TrimSpace(thread.Title)
	threadContext, err := s.threadContext(thread.ID)
	if err != nil {
		s.config.Logf("regenerate thread title: build context for thread %s: %v", thread.ID, err)
		return err
	}
	if threadContext == "" {
		return nil
	}

	title, err := s.generate(thread, threadtitle.BuildRegeneratePrompt(previousTitle, threadContext), nil)
	if err != nil {
		s.config.Logf("regenerate thread title: %s", textgen.RedactError(err))
		return err
	}
	if title == "" || title == threadtitle.Default || title == previousTitle {
		return nil
	}
	if _, err := s.applyIfCurrent(thread.ID, thread.Title, title); err != nil {
		s.config.Logf("regenerate thread title: apply title for thread %s: %v", thread.ID, err)
		return err
	}
	return nil
}

func (s *Service) generate(thread store.Thread, prompt string, imagePaths []string) (string, error) {
	if s.config.Generate == nil {
		return "", fmt.Errorf("generate thread title: provider generator unavailable")
	}
	raw, err := s.config.Generate(thread, prompt, imagePaths)
	if err != nil {
		return "", err
	}
	return threadtitle.Sanitize(raw), nil
}

func (s *Service) applyIfCurrent(threadID, expected, title string) (bool, error) {
	if s.config.Store == nil {
		return false, fmt.Errorf("apply thread title: store unavailable")
	}
	updated, err := s.config.Store.UpdateTitleIfCurrent(threadID, expected, title)
	if err != nil || !updated {
		return updated, err
	}
	s.config.Applied(Applied{ThreadID: threadID, Title: title})
	return true, nil
}

func (s *Service) threadContext(threadID string) (string, error) {
	if s.config.Store == nil {
		return "", fmt.Errorf("thread title context: store unavailable")
	}
	items, dropped, err := s.config.Store.ThreadTitleContextItems(threadID, contextItemLimit)
	if err != nil {
		return "", err
	}
	messages := make([]threadtitle.Message, 0, len(items))
	for _, item := range items {
		message := threadtitle.Message{Role: item.Role, Text: item.Summary}
		if item.Kind == "user_text" {
			meta, err := usermessage.FromItem(item)
			if err != nil {
				s.config.Logf("regenerate thread title: decode user meta for item %s: %v", item.ID, err)
			}
			for _, attachment := range meta.Attachments {
				message.AttachmentNames = append(message.AttachmentNames, attachment.Filename)
			}
		}
		messages = append(messages, message)
	}
	return threadtitle.FormatThreadContext(messages, dropped), nil
}

func (s *Service) imagePaths(threadID string, attachments []store.Attachment) ([]string, error) {
	if len(attachments) == 0 || s.config.Attachments == nil {
		return nil, nil
	}
	paths := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		if !strings.HasPrefix(attachment.MimeType, "image/") {
			continue
		}
		record, path, ok, err := s.config.Attachments.Get(attachment.ID)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", attachment.ID, err)
		}
		if !ok || record.ThreadID != threadID {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}
