package agy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

const (
	// DefaultCatalogTTL matches Codex's live model catalog TTL.
	DefaultCatalogTTL = 10 * time.Minute
	// DefaultContextWindow is Antigravity's standard 1 MiB Gemini window.
	DefaultContextWindow = 1048576
)

// CatalogCache caches live-probed Antigravity model catalogs keyed by binary.
type CatalogCache struct {
	mu       sync.Mutex
	entries  map[string]catalogEntry
	ttl      time.Duration
	disabled bool
}

type catalogEntry struct {
	models    []provider.ModelInfo
	fetchedAt time.Time
}

func NewCatalogCache() *CatalogCache {
	return &CatalogCache{
		entries: make(map[string]catalogEntry),
		ttl:     DefaultCatalogTTL,
	}
}

// NewDisabledCatalogCache returns a cache whose Disabled() reports true so
// tests never spawn the real agy binary for live catalog probes.
func NewDisabledCatalogCache() *CatalogCache {
	return &CatalogCache{
		entries:  make(map[string]catalogEntry),
		ttl:      DefaultCatalogTTL,
		disabled: true,
	}
}

func (c *CatalogCache) Disabled() bool {
	return c != nil && c.disabled
}

func (c *CatalogCache) Get(key string) ([]provider.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.fetchedAt) > c.ttl {
		return nil, false
	}
	return provider.CloneModels(e.models), true
}

func (c *CatalogCache) Put(key string, models []provider.ModelInfo) {
	c.mu.Lock()
	c.entries[key] = catalogEntry{
		models:    provider.CloneModels(models),
		fetchedAt: time.Now(),
	}
	c.mu.Unlock()
}

func (c *CatalogCache) Clear() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}

// ProbeCatalogOnConn performs a zero-token model x thought_level x contextWindow
// discovery pass on an already-live *acp.Conn. Opens one temporary session,
// cycles session/set_config_option(model=...) across each offered model (no
// prompts, zero tokens), collects each model's offered thought_level values and
// usage_update context window size, and closes the temporary session.
func ProbeCatalogOnConn(ctx context.Context, conn *acp.Conn, workDir string) ([]provider.ModelInfo, error) {
	openRes, err := conn.NewRawSession(ctx, acp.SessionNewParams{
		CWD:        workDir,
		MCPServers: []acp.MCPServerConfig{},
	})
	if err != nil {
		return nil, fmt.Errorf("agy catalog: session/new: %w", err)
	}
	sessionID := openRes.SessionID
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.CloseRawSession(closeCtx, sessionID)
		cancel()
	}()

	modelOpt, ok := findConfigOption(openRes.ConfigOptions, "model")
	if !ok || len(modelOpt.Options) == 0 {
		// Fall back to legacy openRes.Models if configOptions was absent.
		if openRes.Models != nil && len(openRes.Models.AvailableModels) > 0 {
			out := make([]provider.ModelInfo, 0, len(openRes.Models.AvailableModels))
			for _, m := range openRes.Models.AvailableModels {
				out = append(out, provider.ModelInfo{
					Slug:           m.ModelID,
					Name:           m.Name,
					Provider:       string(provider.Agy),
					IsDefault:      m.ModelID == openRes.Models.CurrentModelID,
					ContextWindows: agyContextWindowOptions(DefaultContextWindow),
				})
			}
			return out, nil
		}
		return nil, nil
	}

	defaultModelID := modelOpt.CurrentValue
	currentOpts := openRes.ConfigOptions
	currentSize := DefaultContextWindow
	isUsageSize := func(u acp.SessionUpdate) bool {
		return u.SessionUpdate == "usage_update" && u.Size > 0
	}
	out := make([]provider.ModelInfo, 0, len(modelOpt.Options))
	var lastSwitchErr error
	for _, choice := range modelOpt.Options {
		cap := conn.StartUpdateCapture(sessionID, 32)
		if choice.Value != currentOptionValue(currentOpts, "model") {
			updated, setErr := conn.SetConfigOption(ctx, sessionID, "model", choice.Value)
			if setErr != nil {
				cap.Stop()
				lastSwitchErr = setErr
				continue
			}
			if len(updated) > 0 {
				currentOpts = updated
			}
			// Real agy emits `usage_update` asynchronously right after the
			// `session/set_config_option` reply whenever the model's context
			// window size changes. Wait briefly so the size is attributed to
			// this model rather than spilling into the next iteration.
			cap.WaitFor(ctx, isUsageSize, 80*time.Millisecond)
		}
		updates := cap.Stop()
		for _, u := range updates {
			if isUsageSize(u) {
				currentSize = u.Size
			}
		}

		var efforts []provider.ReasoningEffortOption
		if thoughtOpt, hasThought := findConfigOption(currentOpts, "thought_level"); hasThought {
			for _, tChoice := range thoughtOpt.Options {
				efforts = append(efforts, provider.NewReasoningEffortOptionWithLabel(
					tChoice.Value,
					tChoice.Name,
					tChoice.Value == thoughtOpt.CurrentValue,
				))
			}
		}

		name := strings.TrimSpace(choice.Name)
		if name == "" {
			name = choice.Value
		}
		out = append(out, provider.ModelInfo{
			Slug:             choice.Value,
			Name:             name,
			Provider:         string(provider.Agy),
			IsDefault:        choice.Value == defaultModelID,
			ContextWindows:   agyContextWindowOptions(currentSize),
			ReasoningEfforts: efforts,
		})
	}
	if len(out) == 0 && lastSwitchErr != nil {
		return nil, fmt.Errorf("agy: probe catalog model switches failed: %w", lastSwitchErr)
	}
	return out, nil
}

// ProbeCatalog dials a temporary agy acp server when no pooled connection is
// live, probes the model catalog, and closes the temporary server.
func ProbeCatalog(ctx context.Context, binary, workDir string, customEnv map[string]string, reaper procutil.Reaper) ([]provider.ModelInfo, error) {
	env, unset := LaunchEnv(customEnv, nil)
	conn, err := acp.Dial(ctx, acp.ConnConfig{
		Binary:       ResolveBinary(binary),
		Args:         LaunchArgs("", ""),
		WorkDir:      workDir,
		Env:          env,
		UnsetEnv:     unset,
		Reaper:       reaper,
		ProviderName: string(provider.Agy),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return ProbeCatalogOnConn(ctx, conn, workDir)
}

func findConfigOption(opts []acp.ConfigOption, id string) (acp.ConfigOption, bool) {
	for _, o := range opts {
		if o.ID == id {
			return o, true
		}
	}
	return acp.ConfigOption{}, false
}

func currentOptionValue(opts []acp.ConfigOption, id string) string {
	if o, ok := findConfigOption(opts, id); ok {
		return o.CurrentValue
	}
	return ""
}

func agyContextWindowOptions(tokens int) []provider.ContextWindowOption {
	if tokens <= 0 {
		tokens = DefaultContextWindow
	}
	label := formatContextTokensLabel(tokens)
	tier := provider.ContextTierStandard
	if tokens >= 1000000 {
		tier = provider.ContextTierExtended
	}
	return []provider.ContextWindowOption{{
		Tokens:  tokens,
		Label:   label,
		Tier:    tier,
		Default: true,
	}}
}

func formatContextTokensLabel(tokens int) string {
	if tokens >= 1000000 {
		return "1m"
	}
	return fmt.Sprintf("%dk", tokens/1000)
}
