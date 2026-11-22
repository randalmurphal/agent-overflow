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
	announceModels()
}

var (
	reportMu sync.Mutex
	reported = make(chan struct{})
)

// ModelsReported is closed by the next model answer stored or seeded, for
// any identity. A caller waiting for a probe to back its list takes it
// before it reads Models, so an answer that lands between the two still
// wakes it, and reads Models again when it closes.
func ModelsReported() <-chan struct{} {
	reportMu.Lock()
	defer reportMu.Unlock()
	return reported
}

func announceModels() {
	reportMu.Lock()
	close(reported)
	reported = make(chan struct{})
	reportMu.Unlock()
}

// DropModelsForBinary forgets every model answer learned from one configured
// binary path, returning how many identities it dropped. Called by the
// provider-binary watcher when the file behind the path reports a new
// version: learned models and flags survive degraded probe answers
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

// ModelAliases returns what each alias the Claude CLI reported for one probe
// identity resolves to (claudemodels.Catalog.AliasesFor).
func ModelAliases(key provider.ProbeCacheKey) map[string]string {
	return modelCatalog().AliasesFor(key)
}

// Models returns the picker catalog for one Claude-family provider, and
// what backs it: the shipped list, the same binary's newest report, or
// this identity's own (claudemodels.Backing).
func Models(key provider.ProbeCacheKey, providerName string) (models []provider.ModelInfo, backing claudemodels.Backing) {
	return modelCatalog().ModelsFor(key, providerName)
}

// Export returns the evidence behind one identity's model entry so the
// application layer can persist it per account.
func Export(key provider.ProbeCacheKey) (claudemodels.Snapshot, bool) {
	return modelCatalog().Export(key)
}

// Seed restores a persisted model answer for one identity at boot. The caller
// owns checking that the snapshot still describes the binary behind the key.
func Seed(key provider.ProbeCacheKey, snapshot claudemodels.Snapshot) {
	modelCatalog().Seed(key, snapshot)
	announceModels()
}
