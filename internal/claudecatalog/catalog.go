package claudecatalog

import (
	"log"
	"sync"

	"agent-overflow/internal/claudecommands"
	"agent-overflow/internal/claudemodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

var (
	mu       sync.Mutex
	commands *claudecommands.Cache
	models   *claudemodels.Catalog
)

func commandCache() *claudecommands.Cache {
	mu.Lock()
	defer mu.Unlock()
	if commands == nil {
		commands = claudecommands.NewCache()
	}
	return commands
}

func modelCatalog() *claudemodels.Catalog {
	mu.Lock()
	defer mu.Unlock()
	if models == nil {
		models = claudemodels.NewCatalog()
	}
	return models
}

// Reset clears both probe-enriched answers. One Claude initialize response
// fills them together, so resetting only one would allow mixed identities.
func Reset() {
	mu.Lock()
	commands = claudecommands.NewCache()
	models = claudemodels.NewCatalog()
	mu.Unlock()
}

// CommandCapture records whether a probe reported a commands field as well
// as the field's value. A missing report must not clear an earlier answer.
type CommandCapture struct {
	reported bool
	commands []provider.SlashCommand
	err      error
}

func (c *CommandCapture) Capture(commands []provider.SlashCommand, err error) {
	c.reported = true
	c.commands = commands
	c.err = err
}

func (c CommandCapture) Store(key provider.ProbeCacheKey) {
	if c.reported {
		commandCache().Store(key, c.commands, c.err)
	}
}

// ModelCapture is the model-list counterpart to CommandCapture.
type ModelCapture struct {
	reported bool
	models   []claude.WireModel
	err      error
}

func (c *ModelCapture) Capture(models []claude.WireModel, err error) {
	c.reported = true
	c.models = models
	c.err = err
}

func (c ModelCapture) Store(key provider.ProbeCacheKey) {
	if !c.reported {
		return
	}
	if drift := modelCatalog().Store(key, c.models, c.err); len(drift) > 0 {
		log.Printf("claude model catalog: %s", claudemodels.FormatDrift(drift))
	}
}

// DropModelsForBinary forgets every model answer learned from one configured
// binary path, returning how many identities it dropped. Called by the
// provider-binary watcher when the file behind the path reports a new
// version: learned wire-only models survive degraded probe answers
// (claudemodels.DriftRetained), so a binary change is the one event that may
// subtract them — and the watcher re-probes immediately after. The command
// cache is left alone: it is replace-wholesale per probe, so that same
// re-probe replaces it anyway.
func DropModelsForBinary(binary string) int {
	return modelCatalog().DropBinary(binary)
}

// Commands returns the slash-command answer for one probe identity.
func Commands(key provider.ProbeCacheKey) ([]provider.SlashCommand, bool) {
	return commandCache().AnswerFor(key)
}

// Models returns the picker catalog for one Claude-family provider.
func Models(key provider.ProbeCacheKey, providerName string) []provider.ModelInfo {
	return modelCatalog().ModelsFor(key, providerName)
}
