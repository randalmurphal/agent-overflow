package main

import (
	"log"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
)

// claimThreadTitleGeneration reserves the thread for one title
// generation run and reports whether the caller got it. Every generation
// goroutine — automatic first-turn, auto-heal on a later send, and
// user-triggered regeneration — claims BEFORE it starts and releases in
// its defer.
//
// Without the claim, a thread that still carries the default title
// starts a fresh generation on every send, and each one is up to two
// provider CLI attempts of threadtitle.Timeout: five quick sends into a
// slow provider is ten concurrent subprocesses racing to write one
// title. A caller that loses the claim joins the running generation
// rather than queueing behind it — the running goroutine's completion
// event is the answer for both.
func (a *App) claimThreadTitleGeneration(threadID string) bool {
	a.threadTitleGenMu.Lock()
	defer a.threadTitleGenMu.Unlock()
	if _, held := a.threadTitleGenActive[threadID]; held {
		return false
	}
	if a.threadTitleGenActive == nil {
		a.threadTitleGenActive = make(map[string]struct{})
	}
	a.threadTitleGenActive[threadID] = struct{}{}
	return true
}

func (a *App) releaseThreadTitleGeneration(threadID string) {
	a.threadTitleGenMu.Lock()
	defer a.threadTitleGenMu.Unlock()
	delete(a.threadTitleGenActive, threadID)
}

// emitThreadTitleGeneration publishes the completion frame of one
// generation run. It is emitted AFTER any thread:updated patch the
// compare-and-swap produced, so a frontend that clears its pending state
// on this frame has already received the new title.
func (a *App) emitThreadTitleGeneration(threadID string, err error) {
	message := ""
	if err != nil {
		message = textgen.RedactError(err)
	}
	a.emitEvent("thread:title_generation", ThreadTitleGenerationEvent{
		ThreadID: threadID,
		Error:    message,
	})
}

// runClaimedThreadTitleGeneration is the goroutine shell every generation
// runs in: the body produces (and CASes) the title, the shell owns the
// claim's release and the single completion emit. The defers run LIFO on
// purpose — the RELEASE fires before the EMIT — so a caller that claims in
// the window between them starts a run of its own and gets its own event,
// instead of "joining" a run whose completion frame has already gone out.
func (a *App) runClaimedThreadTitleGeneration(threadID string, body func() error) {
	var runErr error
	defer func() { a.emitThreadTitleGeneration(threadID, runErr) }()
	defer a.releaseThreadTitleGeneration(threadID)
	runErr = body()
}

// maybeGenerateThreadTitleWithAttachments kicks off background title
// generation for a thread that still carries the default title. It is
// deliberately NOT gated on "this is the first turn": a thread whose
// first-turn generation failed (provider down, usage limit, timeout)
// would otherwise stay "New Thread" forever, so the next send retries.
// applyThreadTitleIfCurrent's compare-and-swap is what keeps that safe —
// a user rename wins, and two racing generations cannot clobber.
//
// hasPriorItems picks the PROMPT. A true first turn is titled from the
// message the user just sent; a heal on a later send has a whole
// conversation to read, and titling one mid-thread message with the
// first-turn prompt would name the tangent instead of the thread.
func (a *App) maybeGenerateThreadTitleWithAttachments(
	thread store.Thread,
	content string,
	attachments []store.Attachment,
	hasPriorItems bool,
) {
	// Known carve-out: the gate reads the TITLE, so a thread the user
	// deliberately renamed to "New Thread" is indistinguishable from one
	// that was never titled and gets re-titled on a later send. Telling
	// them apart needs a persisted "user named this" bit, which the rename
	// path does not carry today; the sentinel-as-chosen-name shape is rare
	// enough that the heal's value wins.
	if strings.TrimSpace(thread.Title) != threadtitle.Default {
		return
	}
	if strings.TrimSpace(content) == "" {
		return
	}
	if !a.claimThreadTitleGeneration(thread.ID) {
		// A generation for this thread is already running. Its completion
		// event covers this send too.
		return
	}

	go a.runClaimedThreadTitleGeneration(thread.ID, func() error {
		title, err := a.autoThreadTitle(thread, content, attachments, hasPriorItems)
		if err != nil {
			log.Printf("send message: generate thread title: %s", textgen.RedactError(err))
			return err
		}
		if title == "" || title == threadtitle.Default {
			return nil
		}
		// CAS against the RAW stored title, not the Default constant: the
		// gate above trims, so a stored title of "  New Thread  " reaches
		// here and a byte-exact swap against the constant could never
		// apply.
		if _, err := a.applyThreadTitleIfCurrent(thread.ID, thread.Title, title); err != nil {
			log.Printf("send message: apply generated thread title: %v", err)
			return err
		}
		return nil
	})
}

// autoThreadTitle drafts the title for an automatic run, choosing the
// prompt from whether the thread already has history.
//
// The heal path falls back to the first-turn prompt when the context
// read fails or renders nothing: a thread whose rows are all tool
// traffic has no transcript to read, and the message in hand is still
// better than leaving the thread "New Thread". A read error is logged
// rather than returned for the same reason — the fallback is a real
// answer, not a way of hiding the failure.
func (a *App) autoThreadTitle(
	thread store.Thread,
	content string,
	attachments []store.Attachment,
	hasPriorItems bool,
) (string, error) {
	if hasPriorItems {
		threadContext, err := a.threadTitleContext(thread.ID)
		if err != nil {
			log.Printf("generate thread title: build context for thread %s: %v", thread.ID, err)
		}
		if threadContext != "" {
			// The stored title is the generic default here, which the
			// regeneration prompt handles: it is told to replace a previous
			// title that is generic.
			return a.regeneratedThreadTitle(thread, strings.TrimSpace(thread.Title), threadContext)
		}
	}
	return a.generatedThreadTitle(thread, content, attachments)
}

// runThreadTitleRegeneration is the body of a user-triggered
// regeneration, running inside runClaimedThreadTitleGeneration's shell.
// The returned error is what the completion frame carries; every nil
// return is a clean completion, no-op outcomes included.
func (a *App) runThreadTitleRegeneration(thread store.Thread) error {
	// The compare-and-swap matches the stored bytes exactly, so the raw
	// title is what it expects; the trimmed form is what the prompt and
	// the "did this actually change" check use.
	previousTitle := strings.TrimSpace(thread.Title)

	threadContext, err := a.threadTitleContext(thread.ID)
	if err != nil {
		log.Printf("regenerate thread title: build context for thread %s: %v", thread.ID, err)
		return err
	}
	if threadContext == "" {
		// Nothing to title from. Not an error — a thread whose only
		// content is tool traffic has no subject to name.
		return nil
	}

	title, err := a.regeneratedThreadTitle(thread, previousTitle, threadContext)
	if err != nil {
		log.Printf("regenerate thread title: %s", textgen.RedactError(err))
		return err
	}
	if title == "" || title == threadtitle.Default || title == previousTitle {
		// The model had nothing better to say.
		return nil
	}

	// Either outcome of the swap is a clean completion: losing to a
	// rename or a concurrent generation means the other writer's title is
	// the truth, which is not a failure of this run.
	if _, err := a.applyThreadTitleIfCurrent(thread.ID, thread.Title, title); err != nil {
		log.Printf("regenerate thread title: apply title for thread %s: %v", thread.ID, err)
		return err
	}
	return nil
}
