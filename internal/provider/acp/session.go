package acp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"
)

const (
	chunkCoalesceWindow    = 35 * time.Millisecond
	chunkCoalesceBytes     = 2048
	interruptSettleTimeout = 5 * time.Second
)

// SessionConfig holds thread-scoped configuration for opening an ACP Session on
// a shared or dedicated Conn.
type SessionConfig struct {
	WorkDir               string
	ResumeSessionID       string
	Model                 string
	ReasoningEffort       string
	RuntimeMode           provider.RuntimeMode
	InteractionMode       provider.InteractionMode
	SystemPrompt          string
	AppendSystemPrompt    string
	AdditionalDirectories []string
	DisabledTools         []string
	MCPServers            []MCPServerConfig
	Flavor                Flavor
}

// LiveUpdate describes config changes that apply live on a running ACP Session
// without restarting it.
type LiveUpdate struct {
	Model           string
	ReasoningEffort string
	RuntimeMode     provider.RuntimeMode
	InteractionMode provider.InteractionMode
	HasModel        bool
	HasEffort       bool
	HasRuntimeMode  bool
	HasInteraction  bool
}

// PlanLiveUpdate compares launch options against target options and returns
// (update, true) when every difference can be applied live over ACP, or
// (zero, false) when a spawn-only axis changed and a restart is required.
//
// Resume, ResumeAt, and ForkSession are session lifecycle state, not config:
// a fresh thread records its SessionRef after EventInit, so those fields
// legitimately drift from LaunchOptions and are excluded from the comparison.
func PlanLiveUpdate(from, to provider.SessionOptions) (LiveUpdate, bool) {
	if from.WorkDir != to.WorkDir ||
		from.SystemPrompt != to.SystemPrompt ||
		!slices.Equal(from.DisabledTools, to.DisabledTools) {
		return LiveUpdate{}, false
	}
	var u LiveUpdate
	if from.Model != to.Model {
		u.Model = to.Model
		u.HasModel = true
	}
	if from.ReasoningEffort != to.ReasoningEffort {
		u.ReasoningEffort = string(to.ReasoningEffort)
		u.HasEffort = true
	}
	if from.RuntimeMode != to.RuntimeMode {
		u.RuntimeMode = to.RuntimeMode
		u.HasRuntimeMode = true
	}
	if from.Mode != to.Mode {
		u.InteractionMode = to.Mode
		u.HasInteraction = true
	}
	return u, true
}

type pendingPermission struct {
	rpcID       json.RawMessage
	optionByKey map[string]string
	isUserInput bool
}

type pendingToolQuestion struct {
	toolCallID string
	questions  []provider.UserInputQuestion
	answering  bool
}

type pendingPromptConfirm struct {
	uuid      string
	content   string
	confirmed atomic.Bool
}

type chunkBuffer struct {
	kind  provider.EventKind // EventTextDelta or EventThinking
	buf   strings.Builder
	timer *time.Timer
}

// Session represents one ACP conversation multiplexed onto a *Conn.
// Implements provider.Session.
type Session struct {
	conn            *Conn
	releaseConn     func()
	releaseConnOnce sync.Once

	threadID      string
	instanceID    string
	workDir       string
	flavor        Flavor
	onEvent       func(provider.ProviderEvent)
	approvals     provider.ApprovalRegistry
	disabledTools map[string]struct{}

	// questionActionMu serializes ask_question answer dispatch, ordinary Send
	// replies, and Interrupt so an interrupt or Send can never steal a pending
	// question mid-write or leave an orphaned follow-up prompt.
	questionActionMu sync.Mutex

	mu                     sync.Mutex
	sessionID              string
	model                  string
	reasoningEffort        string
	runtimeMode            provider.RuntimeMode
	interactionMode        provider.InteractionMode
	permissionPolicy       PermissionPolicy
	availableModes         []SessionMode
	configOptions          []ConfigOption
	currentModeID          string
	lastRequestedModeID    string
	lastContextSize        int
	lastUsedTokens         int
	lastCompactionIdx      int
	lastPromptCompact      bool
	inFlightPrompts        int
	wireInFlightPrompts    int
	cancelling             bool
	inFlightZeroCh         chan struct{}
	inFlightConfirms       []*pendingPromptConfirm
	armedConfirm           *pendingPromptConfirm
	turnOpen               bool
	spontaneousTurnOpen    bool
	turnSeq                int
	activeTurnID           string
	turnBeforeToken        any
	turnBeforeSet          bool
	promptUsage            provider.TokenUsage
	promptUsageSeen        bool
	turnHasOutputSchema    bool
	turnLastAssistantText  strings.Builder
	activeBlockKind        provider.EventKind
	startedTools           map[string]NormalizedToolStart
	childStartedTools      map[string]NormalizedToolStart
	childLaunchByConvID    map[string]string
	trackedSubagents       map[string]SubagentLaunchContext
	activeSubagentWatchers int
	pendingPerms           map[string]pendingPermission
	pendingToolQuestions   map[string]pendingToolQuestion
	chunk                  chunkBuffer
	promptWG               sync.WaitGroup
	subagentWG             sync.WaitGroup

	sessionCtx      context.Context
	sessionCancel   context.CancelFunc
	closing         atomic.Bool
	teardownEmitted atomic.Bool
	readDone        chan struct{}
	readDoneOnce    sync.Once
}

var _ provider.Session = (*Session)(nil)

// Open attaches a new or resumed Session to this Conn. Closing the returned
// Session closes only the conversation, leaving the Conn alive for sibling
// sessions.
func (c *Conn) Open(ctx context.Context, threadID string, cfg SessionConfig, onEvent func(provider.ProviderEvent)) (*Session, error) {
	return c.open(ctx, threadID, cfg, onEvent, nil)
}

// OpenPooled attaches a Session to this shared Conn and invokes releaseConn
// when the session closes (or if Open fails).
func (c *Conn) OpenPooled(ctx context.Context, threadID string, cfg SessionConfig, onEvent func(provider.ProviderEvent), releaseConn func()) (*Session, error) {
	return c.open(ctx, threadID, cfg, onEvent, releaseConn)
}

func (c *Conn) open(
	ctx context.Context,
	threadID string,
	cfg SessionConfig,
	onEvent func(provider.ProviderEvent),
	releaseConn func(),
) (*Session, error) {
	flavor := cfg.Flavor
	if flavor == nil {
		flavor = c.flavor
	}
	if flavor == nil {
		flavor = DefaultFlavor{Kind: provider.ProviderKind(c.providerName)}
	}
	rtMode := provider.NormalizeRuntimeMode(string(cfg.RuntimeMode))
	imMode := cfg.InteractionMode
	if imMode == "" {
		imMode = provider.ModeChat
	}

	requestedModel := strings.TrimSpace(cfg.Model)
	requestedEffort := strings.TrimSpace(cfg.ReasoningEffort)
	_, initialPolicy := flavor.MapRuntimeMode(rtMode, imMode, nil)

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	disabledTools := make(map[string]struct{}, len(cfg.DisabledTools))
	for _, tool := range cfg.DisabledTools {
		if t := strings.ToLower(strings.TrimSpace(tool)); t != "" {
			disabledTools[t] = struct{}{}
		}
	}

	s := &Session{
		conn:                 c,
		releaseConn:          releaseConn,
		threadID:             threadID,
		instanceID:           newSessionInstanceID(),
		workDir:              cfg.WorkDir,
		flavor:               flavor,
		onEvent:              onEvent,
		disabledTools:        disabledTools,
		runtimeMode:          rtMode,
		interactionMode:      imMode,
		permissionPolicy:     initialPolicy,
		lastCompactionIdx:    -1,
		startedTools:         make(map[string]NormalizedToolStart),
		childStartedTools:    make(map[string]NormalizedToolStart),
		childLaunchByConvID:  make(map[string]string),
		trackedSubagents:     make(map[string]SubagentLaunchContext),
		pendingPerms:         make(map[string]pendingPermission),
		pendingToolQuestions: make(map[string]pendingToolQuestion),
		sessionCtx:           sessionCtx,
		sessionCancel:        sessionCancel,
		readDone:             make(chan struct{}),
	}

	mcpServers := cfg.MCPServers
	if mcpServers == nil {
		mcpServers = []MCPServerConfig{}
	}
	meta := flavor.BuildSessionMeta(cfg)

	var rawOpen json.RawMessage
	var err error
	resuming := strings.TrimSpace(cfg.ResumeSessionID) != ""
	if resuming {
		// Pre-register before session/resume so immediate notifications route here,
		// refusing atomically if a ReplayLoad currently owns this native sessionId.
		s.mu.Lock()
		s.sessionID = cfg.ResumeSessionID
		s.mu.Unlock()
		if !c.registerSession(cfg.ResumeSessionID, s) {
			sessionCancel()
			if releaseConn != nil {
				s.releasePoolRef()
			}
			return nil, ErrSessionBusy
		}

		rawOpen, err = c.sendRequest(ctx, "session/resume", SessionResumeParams{
			SessionID:             cfg.ResumeSessionID,
			CWD:                   cfg.WorkDir,
			MCPServers:            mcpServers,
			AdditionalDirectories: cfg.AdditionalDirectories,
			Meta:                  meta,
		})
	} else {
		rawOpen, err = c.sendSessionNewRequest(ctx, SessionNewParams{
			CWD:                   cfg.WorkDir,
			MCPServers:            mcpServers,
			AdditionalDirectories: cfg.AdditionalDirectories,
			Meta:                  meta,
		}, s)
	}
	cleanupOpen := func(closeRemote bool, sid string) {
		sessionCancel()
		// dispatchLine may already have registered a fresh sessionId on s even
		// if ctx cancelled or decoding failed before openRes was unpacked.
		s.mu.Lock()
		registeredSID := s.sessionID
		s.mu.Unlock()
		if sid == "" {
			sid = registeredSID
		}
		if sid != "" && (closeRemote || (!resuming && registeredSID != "")) {
			closeCtx, cancel := context.WithTimeout(context.Background(), defaultCloseTimeout)
			_, _ = c.sendRequest(closeCtx, "session/close", SessionCloseParams{SessionID: sid})
			cancel()
		}
		if sid != "" {
			c.unregisterSession(sid, s)
		}
		if releaseConn != nil {
			s.releasePoolRef()
		}
	}
	if err != nil {
		cleanupOpen(false, cfg.ResumeSessionID)
		return nil, err
	}

	var openRes SessionOpenResult
	if err := json.Unmarshal(rawOpen, &openRes); err != nil {
		cleanupOpen(true, cfg.ResumeSessionID)
		return nil, fmt.Errorf("acp: decode session open response: %w", err)
	}
	sessionID := openRes.SessionID
	if sessionID == "" {
		sessionID = cfg.ResumeSessionID
	}
	if sessionID == "" {
		cleanupOpen(true, "")
		return nil, errors.New("acp: session open response missing sessionId")
	}

	s.mu.Lock()
	s.sessionID = sessionID
	s.configOptions = append([]ConfigOption(nil), openRes.ConfigOptions...)
	if openRes.Modes != nil {
		s.availableModes = append([]SessionMode(nil), openRes.Modes.AvailableModes...)
		s.currentModeID = openRes.Modes.CurrentModeID
	}
	_, s.permissionPolicy = flavor.MapRuntimeMode(rtMode, imMode, s.availableModes)
	if serverModel := currentConfigValue(s.configOptions, "model"); serverModel != "" {
		s.model = serverModel
	} else if openRes.Models != nil && openRes.Models.CurrentModelID != "" {
		s.model = openRes.Models.CurrentModelID
	}
	if serverEffort := currentConfigValue(s.configOptions, "thought_level"); serverEffort != "" {
		s.reasoningEffort = serverEffort
	}
	s.mu.Unlock()
	c.registerSession(sessionID, s)

	// Re-apply desired model, thought_level, and mode immediately (required on
	// both fresh sessions and cold session/resume where servers reset to defaults).
	if err := s.applyConfigAxes(ctx, requestedModel, requestedEffort, rtMode, imMode, requestedModel != "", requestedEffort != "", true, false, false); err != nil {
		cleanupOpen(true, sessionID)
		return nil, fmt.Errorf("acp: apply initial session config: %w", err)
	}

	compactionCursor := flavor.InitCompactionCursor(ctx, sessionID)

	s.mu.Lock()
	s.lastCompactionIdx = compactionCursor
	effectiveModel := s.model
	if effectiveModel == "" {
		effectiveModel = currentConfigValue(s.configOptions, "model")
		if effectiveModel == "" && openRes.Models != nil {
			effectiveModel = openRes.Models.CurrentModelID
		}
		s.model = effectiveModel
	}
	s.mu.Unlock()

	initMeta, _ := json.Marshal(provider.SessionInfo{
		SessionID: sessionID,
		Model:     effectiveModel,
		CWD:       cfg.WorkDir,
		Version:   c.agentInfo.Version,
	})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventInit,
		ThreadID:  threadID,
		Meta:      initMeta,
		Timestamp: time.Now(),
	})

	return s, nil
}

// SessionID returns the ACP server's sessionId.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// ConfigOptions returns a copy of the latest ACP configOptions reported for this session.
func (s *Session) ConfigOptions() []ConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ConfigOption(nil), s.configOptions...)
}

func (s *Session) PID() int {
	if s == nil || s.conn == nil {
		return 0
	}
	return s.conn.PID()
}

// ServerVersion returns the ACP agentInfo.version reported during initialize.
func (s *Session) ServerVersion() string {
	if s == nil || s.conn == nil {
		return ""
	}
	return s.conn.AgentInfo().Version
}

func (s *Session) Connection() provider.Connection {
	if s == nil {
		return nil
	}
	return s.conn
}

func (s *Session) OwnedProcesses() []provider.OwnedProcess {
	pid := s.PID()
	if pid <= 0 {
		return nil
	}
	return []provider.OwnedProcess{{PID: pid, PGID: pid, WorkDir: s.workDir}}
}

// ApplyLiveUpdate applies model, reasoning effort, runtime mode, and
// interaction mode changes live on the running session without restarting.
func (s *Session) ApplyLiveUpdate(ctx context.Context, u LiveUpdate) error {
	s.mu.Lock()
	model := s.model
	effort := s.reasoningEffort
	rt := s.runtimeMode
	im := s.interactionMode
	s.mu.Unlock()

	if u.HasModel {
		model = strings.TrimSpace(u.Model)
	}
	if u.HasEffort {
		effort = strings.TrimSpace(u.ReasoningEffort)
	}
	if u.HasRuntimeMode {
		rt = provider.NormalizeRuntimeMode(string(u.RuntimeMode))
	}
	if u.HasInteraction {
		im = u.InteractionMode
	}
	return s.applyConfigAxes(ctx, model, effort, rt, im, u.HasModel, u.HasEffort || u.HasModel, u.HasRuntimeMode || u.HasInteraction, true, u.HasEffort)
}

func (s *Session) applyConfigAxes(
	ctx context.Context,
	model, effort string,
	rt provider.RuntimeMode,
	im provider.InteractionMode,
	applyModel, applyEffort, applyMode, strict, strictEffort bool,
) error {
	s.mu.Lock()
	sessionID := s.sessionID
	availableModes := append([]SessionMode(nil), s.availableModes...)
	opts := append([]ConfigOption(nil), s.configOptions...)
	currentMode := s.currentModeID
	s.mu.Unlock()

	if sessionID == "" {
		return nil
	}

	var errs []error
	if applyModel && model != "" {
		modelApplied := true
		if currentConfigValue(opts, "model") != model {
			newOpts, err := s.conn.SetConfigOption(ctx, sessionID, "model", model)
			if err != nil {
				// Fallback to legacy session/set_model if config option is unsupported.
				if IsMethodNotFound(err) {
					_, legacyErr := s.conn.sendRequest(ctx, "session/set_model", SessionSetModelParams{
						SessionID: sessionID,
						ModelID:   model,
					})
					if legacyErr != nil {
						modelApplied = false
						if !IsMethodNotFound(legacyErr) {
							errs = append(errs, legacyErr)
						} else if strict {
							errs = append(errs, fmt.Errorf("acp: server does not support changing model to %q", model))
						}
					}
				} else {
					errs = append(errs, err)
					modelApplied = false
				}
			} else if len(newOpts) > 0 {
				opts = newOpts
				s.mu.Lock()
				s.configOptions = append([]ConfigOption(nil), newOpts...)
				s.mu.Unlock()
			}
		}
		if modelApplied {
			s.mu.Lock()
			s.model = model
			s.mu.Unlock()
		}
	}

	if applyEffort {
		if effort != "" {
			if optionOffered(opts, "thought_level", effort) {
				effortApplied := true
				if currentConfigValue(opts, "thought_level") != effort {
					newOpts, err := s.conn.SetConfigOption(ctx, sessionID, "thought_level", effort)
					if err != nil {
						errs = append(errs, err)
						effortApplied = false
					} else if len(newOpts) > 0 {
						s.mu.Lock()
						s.configOptions = append([]ConfigOption(nil), newOpts...)
						s.mu.Unlock()
					}
				}
				if effortApplied {
					s.mu.Lock()
					s.reasoningEffort = effort
					s.mu.Unlock()
				}
			} else {
				// Server does not offer this effort (or thought_level at all):
				// reflect the server's actual active value rather than claiming
				// an unoffered effort took effect. Only fail when the caller
				// explicitly asked to change thought_level; a model switch onto
				// a model without that tier falls back to the model's default
				// without forcing a session restart.
				s.mu.Lock()
				s.reasoningEffort = currentConfigValue(opts, "thought_level")
				s.mu.Unlock()
				if strict && strictEffort {
					errs = append(errs, fmt.Errorf("acp: thought_level %q is not offered by server", effort))
				}
			}
		} else {
			s.mu.Lock()
			s.reasoningEffort = currentConfigValue(opts, "thought_level")
			s.mu.Unlock()
		}
	}

	if applyMode {
		targetModeID, policy := s.flavor.MapRuntimeMode(rt, im, availableModes)
		modeApplied := true
		if targetModeID == "" && (im == provider.ModePlan || (len(availableModes) > 0 && strict)) {
			modeApplied = false
			errs = append(errs, fmt.Errorf("acp: server does not advertise a mode matching runtime=%s interaction=%s", rt, im))
		} else if targetModeID != "" && targetModeID != currentMode {
			s.mu.Lock()
			s.lastRequestedModeID = targetModeID
			s.mu.Unlock()
			if _, err := s.conn.sendRequest(ctx, "session/set_mode", SessionSetModeParams{
				SessionID: sessionID,
				ModeID:    targetModeID,
			}); err != nil {
				s.mu.Lock()
				if s.lastRequestedModeID == targetModeID {
					s.lastRequestedModeID = ""
				}
				s.mu.Unlock()
				if !IsMethodNotFound(err) || strict || im == provider.ModePlan {
					errs = append(errs, err)
					modeApplied = false
				}
			} else {
				s.mu.Lock()
				s.currentModeID = targetModeID
				s.mu.Unlock()
			}
		}
		if modeApplied {
			s.mu.Lock()
			s.runtimeMode = rt
			s.interactionMode = im
			s.permissionPolicy = policy
			s.mu.Unlock()
		}
	}

	return errors.Join(errs...)
}

// Send writes a session/prompt request and returns immediately while a tracked
// goroutine awaits the turn's response and emits EventTurnComplete. Calling
// Send while a prompt is already in flight steers the active turn without
// cancelling it.
func (s *Session) Send(ctx context.Context, content string, opts provider.SendOptions) error {
	return s.sendPrompt(ctx, content, opts, false)
}

// Steer sends a mid-turn prompt into the currently active turn. Returns
// ErrNoActiveTurn when no turn is active (or while an interrupt is settling)
// so the flush dispatcher can start a fresh turn instead.
func (s *Session) Steer(ctx context.Context, content string, opts provider.SendOptions) error {
	return s.sendPrompt(ctx, content, opts, true)
}

func (s *Session) sendPrompt(ctx context.Context, content string, opts provider.SendOptions, requireActiveTurn bool) error {
	if s.closing.Load() {
		return errors.New("acp: session is closed")
	}

	s.questionActionMu.Lock()
	defer s.questionActionMu.Unlock()

	if requireActiveTurn {
		s.mu.Lock()
		active := s.turnOpen && !s.cancelling
		s.mu.Unlock()
		if !active {
			return ErrNoActiveTurn
		}
	}

	// Reconcile per-turn InteractionMode (chat vs plan) before sending prompt.
	// Return the error rather than sending under the wrong mode.
	if opts.InteractionMode != "" {
		s.mu.Lock()
		curIM := s.interactionMode
		rt := s.runtimeMode
		s.mu.Unlock()
		if opts.InteractionMode != curIM {
			if err := s.applyConfigAxes(ctx, "", "", rt, opts.InteractionMode, false, false, true, true, false); err != nil {
				return fmt.Errorf("acp: reconcile interaction mode %s: %w", opts.InteractionMode, err)
			}
		}
	}

	hasOutputSchema := len(opts.OutputSchema) > 0
	promptText := content
	if hasOutputSchema {
		promptText = appendOutputSchemaInstruction(content, opts.OutputSchema)
	}
	blocks := buildPromptBlocks(promptText, opts.Attachments)
	if len(blocks) == 0 {
		return errors.New("acp: cannot send empty prompt")
	}

	return s.dispatchPrompt(ctx, blocks, hasOutputSchema, strings.TrimSpace(opts.UserMessageUUID), content, true)
}

func (s *Session) startPromptRequest(sessionID string, blocks []ContentBlock, onResponse func(rawLine []byte)) (func(context.Context) (json.RawMessage, error), error) {
	if sessionID == "" {
		return nil, errors.New("acp: session not initialized")
	}

	// No timeout on session/prompt: turns can run for minutes or hours.
	wait, err := s.conn.startRequestWithOnResponse("session/prompt", SessionPromptParams{
		SessionID: sessionID,
		Prompt:    blocks,
	}, 0, onResponse)
	if err != nil {
		return nil, err
	}
	return wait, nil
}

func (s *Session) emitPromptConfirmation(c *pendingPromptConfirm) {
	if c == nil || c.uuid == "" || s.closing.Load() || !c.confirmed.CompareAndSwap(false, true) {
		return
	}
	confirmMeta, _ := json.Marshal(map[string]any{
		"provider_item_id":                         c.uuid,
		provider.MetaUserTextTranscriptRecordedKey: true,
	})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  s.threadID,
		Role:      "user",
		Content:   c.content,
		Meta:      confirmMeta,
		Timestamp: time.Now(),
	})
}

func (s *Session) removeInFlightConfirmLocked(c *pendingPromptConfirm) {
	if len(s.inFlightConfirms) == 0 {
		return
	}
	for i, entry := range s.inFlightConfirms {
		if entry == c {
			s.inFlightConfirms = append(s.inFlightConfirms[:i], s.inFlightConfirms[i+1:]...)
			return
		}
	}
}

func responseWasCancelledOrError(rawLine []byte) bool {
	if len(rawLine) == 0 {
		return true
	}
	var env struct {
		Error  *json.RawMessage `json:"error"`
		Result *struct {
			StopReason string `json:"stopReason"`
		} `json:"result"`
	}
	if json.Unmarshal(rawLine, &env) != nil {
		return true
	}
	if env.Error != nil && string(*env.Error) != "null" {
		return true
	}
	if env.Result != nil && env.Result.StopReason == "cancelled" {
		return true
	}
	return false
}

func (s *Session) rearmNextConfirmLocked(armNext bool) {
	if !armNext || s.cancelling || s.spontaneousTurnOpen {
		s.armedConfirm = nil
		return
	}
	if len(s.inFlightConfirms) > 0 {
		head := s.inFlightConfirms[0]
		if head != nil && !head.confirmed.Load() {
			s.armedConfirm = head
			return
		}
	}
	s.armedConfirm = nil
}

func (s *Session) makeWirePromptSettler(confirm *pendingPromptConfirm) func(rawLine []byte) {
	var once sync.Once
	return func(rawLine []byte) {
		once.Do(func() {
			armNext := len(rawLine) > 0 && !responseWasCancelledOrError(rawLine)
			s.mu.Lock()
			wasHead := len(s.inFlightConfirms) > 0 && s.inFlightConfirms[0] == confirm
			s.removeInFlightConfirmLocked(confirm)
			if s.armedConfirm == confirm {
				s.armedConfirm = nil
			}
			if s.wireInFlightPrompts > 0 {
				s.wireInFlightPrompts--
			}
			if wasHead || s.wireInFlightPrompts == 1 {
				s.rearmNextConfirmLocked(armNext)
			}
			s.mu.Unlock()
		})
	}
}

func (s *Session) confirmArmedPrompt() {
	s.mu.Lock()
	c := s.armedConfirm
	if c != nil {
		s.armedConfirm = nil
	}
	s.mu.Unlock()
	s.emitPromptConfirmation(c)
}

func (s *Session) signalInFlightZeroLocked() {
	if s.inFlightZeroCh != nil {
		close(s.inFlightZeroCh)
		s.inFlightZeroCh = nil
	}
}

func isCompactPromptText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return trimmed == "/compact" || strings.HasPrefix(trimmed, "/compact ")
}

func (s *Session) dispatchPrompt(ctx context.Context, blocks []ContentBlock, hasOutputSchema bool, userMsgUUID, userContent string, detachQuestions bool) error {
	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	if sessionID == "" {
		return errors.New("acp: session not initialized")
	}

	// Capture any pre-prompt flavor token (e.g. agy SQLite max step idx)
	// BEFORE registering in-flight state, detaching pending questions, or
	// arming armedConfirm so neither delayed updates nor the original
	// prompt's waiter can race this I/O window.
	beforeToken := s.flavor.BeforePrompt(ctx, sessionID)

	confirm := &pendingPromptConfirm{uuid: userMsgUUID, content: userContent}
	isCompact := isCompactPromptText(userContent)

	// Atomically detach pre-existing pending questions AND register in-flight
	// state under the same s.mu lock BEFORE writing session/prompt so:
	// (a) an earlier prompt's finishPrompt cannot observe 0 prompts + 0
	//     questions and prematurely close the held turn,
	// (b) a fast completion of this prompt cannot see the old question and
	//     hold the turn open, and
	// (c) early updates on the shared reader goroutine cannot open a
	//     spontaneous wake turn.
	var openTurnID string
	var prevOutputSchema bool
	var detachedQuestions map[string]pendingToolQuestion
	var detachedTools map[string]NormalizedToolStart
	var detachTurnID string
	s.mu.Lock()
	if detachQuestions && len(s.pendingToolQuestions) > 0 {
		detachedQuestions = make(map[string]pendingToolQuestion, len(s.pendingToolQuestions))
		detachedTools = make(map[string]NormalizedToolStart, len(s.pendingToolQuestions))
		detachTurnID = s.activeTurnID
		for id, q := range s.pendingToolQuestions {
			detachedQuestions[id] = q
			if st, ok := s.startedTools[id]; ok {
				detachedTools[id] = st
				delete(s.startedTools, id)
			}
			delete(s.pendingToolQuestions, id)
		}
	}
	priorWireInFlight := s.wireInFlightPrompts
	s.inFlightPrompts++
	s.wireInFlightPrompts++
	s.cancelling = false
	if isCompact {
		s.lastPromptCompact = true
	}
	s.inFlightConfirms = append(s.inFlightConfirms, confirm)
	if !s.turnBeforeSet {
		s.turnBeforeToken = beforeToken
		s.turnBeforeSet = true
	}
	if !s.turnOpen {
		s.turnSeq++
		s.activeTurnID = s.nextTurnID("turn")
		s.turnOpen = true
		s.spontaneousTurnOpen = false
		s.promptUsage = provider.TokenUsage{}
		s.promptUsageSeen = false
		s.turnHasOutputSchema = hasOutputSchema
		s.turnLastAssistantText.Reset()
		openTurnID = s.activeTurnID
	} else {
		prevOutputSchema = s.turnHasOutputSchema
		if hasOutputSchema {
			s.turnHasOutputSchema = true
		}
	}
	// Arm immediately when no other wire prompt or spontaneous wake is active.
	// Background commands/subagents do not prevent confirming this user prompt
	// on the first parent-scope response update.
	if priorWireInFlight == 0 && !s.spontaneousTurnOpen && confirm.uuid != "" {
		s.armedConfirm = confirm
	} else if priorWireInFlight > 0 {
		// Leave any already-armed predecessor confirm intact; do not overwrite
		// with a newly steered prompt until its predecessor resolves.
	} else {
		s.armedConfirm = nil
	}
	s.mu.Unlock()

	settleWire := s.makeWirePromptSettler(confirm)
	wait, err := s.startPromptRequest(sessionID, blocks, settleWire)
	if err != nil {
		settleWire(nil)
		s.mu.Lock()
		if s.inFlightPrompts > 0 {
			s.inFlightPrompts--
		}
		if s.inFlightPrompts == 0 {
			s.turnBeforeToken = nil
			s.turnBeforeSet = false
			s.lastPromptCompact = false
			s.signalInFlightZeroLocked()
		}
		if openTurnID != "" && s.inFlightPrompts == 0 {
			s.turnOpen = false
			s.turnHasOutputSchema = false
		} else if openTurnID == "" {
			s.turnHasOutputSchema = prevOutputSchema
		}
		for id, q := range detachedQuestions {
			if _, exists := s.pendingToolQuestions[id]; !exists {
				s.pendingToolQuestions[id] = q
			}
		}
		for id, st := range detachedTools {
			if _, exists := s.startedTools[id]; !exists {
				s.startedTools[id] = st
			}
		}
		s.mu.Unlock()
		return err
	}

	if openTurnID != "" {
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  s.threadID,
			TurnID:    openTurnID,
			Timestamp: time.Now(),
		})
	}
	if len(detachedQuestions) > 0 {
		s.emitResolvedDetachedToolQuestions(detachedQuestions, detachTurnID, "answered", map[string]provider.UserInputAnswer{
			"reply": {userContent},
		})
	}

	s.promptWG.Add(1)
	go func() {
		defer s.promptWG.Done()
		rawResp, waitErr := wait(s.sessionCtx)
		settleWire(nil)
		s.finishPrompt(rawResp, waitErr, beforeToken, confirm)
	}()

	return nil
}

func (s *Session) finishPrompt(rawResp json.RawMessage, waitErr error, beforeToken any, confirm *pendingPromptConfirm) {
	var res SessionPromptResult
	if waitErr == nil && len(rawResp) > 0 {
		_ = json.Unmarshal(rawResp, &res)
	}

	closing := s.closing.Load()
	if !closing {
		// Fallback confirmation for prompts that completed cleanly without
		// emitting any intermediate parent update (e.g. /compact or empty reply).
		// Never confirm cancelled or errored prompts.
		if confirm != nil && waitErr == nil && res.StopReason != "cancelled" {
			s.emitPromptConfirmation(confirm)
		}
		s.closeActiveContentBlock()
	} else {
		s.stopChunkTimer()
	}

	s.mu.Lock()
	sessionID := s.sessionID
	model := s.model
	if res.Usage != nil {
		converted := s.flavor.ConvertUsage(*res.Usage, model)
		s.promptUsage.Add(converted)
		s.promptUsageSeen = true
	}
	if s.inFlightPrompts > 0 {
		s.inFlightPrompts--
	}
	remaining := s.inFlightPrompts
	turnID := s.activeTurnID
	effectiveBeforeToken := beforeToken
	if s.turnBeforeSet {
		effectiveBeforeToken = s.turnBeforeToken
	}
	wasCompact := s.lastPromptCompact
	// When the server ends a prompt solely to pause for a pending ask_question
	// tool (e.g. agy emitting tool_call(status="pending") + end_turn before the
	// user's answer prompt), hold the logical turn open so triage keeps
	// pendingUserInputs alive and does not force-close AskUserQuestion as errored.
	holdForToolQuestion := remaining == 0 &&
		waitErr == nil &&
		(res.StopReason == "" || res.StopReason == "end_turn") &&
		len(s.pendingToolQuestions) > 0
	var totalUsage *provider.TokenUsage
	var structuredOutput json.RawMessage
	if remaining == 0 {
		s.cancelling = false
		s.signalInFlightZeroLocked()
	}
	if remaining == 0 && !holdForToolQuestion {
		if s.promptUsageSeen {
			u := s.promptUsage
			totalUsage = &u
		}
		if s.turnHasOutputSchema {
			structuredOutput = extractStructuredJSONObject(s.turnLastAssistantText.String())
		}
		s.promptUsage = provider.TokenUsage{}
		s.promptUsageSeen = false
		s.turnHasOutputSchema = false
		s.turnLastAssistantText.Reset()
		s.turnBeforeToken = nil
		s.turnBeforeSet = false
		s.lastPromptCompact = false
		s.turnOpen = false
		s.spontaneousTurnOpen = false
	}
	s.mu.Unlock()

	if closing || s.closing.Load() {
		return
	}

	if waitErr != nil {
		errMsg := waitErr.Error()
		failure := &provider.FailureMeta{
			Class:    provider.FailureFatal,
			Boundary: provider.FailureBoundaryTurn,
		}
		errMetaFields := map[string]any{}
		if IsAuthRequired(waitErr) {
			failure.Code = "authentication_failed"
			errMetaFields["error"] = "authentication_failed"
		}
		if confirm != nil && confirm.uuid != "" && !confirm.confirmed.Load() {
			errMetaFields["failed_provider_item_id"] = confirm.uuid
		}
		var errMeta json.RawMessage
		if len(errMetaFields) > 0 {
			errMeta, _ = json.Marshal(errMetaFields)
		}
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Content:   errMsg,
			Meta:      errMeta,
			Failure:   failure,
			Timestamp: time.Now(),
		})
		if remaining > 0 {
			return
		}
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnComplete,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Timestamp: time.Now(),
			TurnComplete: &provider.WireTurnCompleteMeta{
				UsageScope:   s.usageScope(),
				StopReason:   "error",
				Usage:        totalUsage,
				ModelUsage:   buildModelUsage(model, totalUsage),
				OwnUsage:     totalUsage,
				ErrorMessage: errMsg,
			},
		})
		return
	}

	if holdForToolQuestion {
		return
	}

	// Another steered prompt is still running for this logical turn; let its
	// completion settle the turn with the combined usage.
	if remaining > 0 {
		return
	}

	if wasCompact {
		s.maybeEmitCompaction(sessionID, turnID, "manual")
	}

	stopReason := res.StopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	// Check flavor out-of-band outcome override (e.g. agy silent step_type=17 errors).
	if override := s.flavor.AfterPrompt(context.Background(), sessionID, stopReason, effectiveBeforeToken); override != nil && override.ErrorMessage != "" {
		failure := override.Failure
		if failure == nil {
			failure = &provider.FailureMeta{
				Class:    provider.FailureTransient,
				Boundary: provider.FailureBoundaryTurn,
			}
		}
		var errMeta json.RawMessage
		if failure.Code != "" {
			errMeta, _ = json.Marshal(map[string]any{"error": failure.Code})
		}
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Content:   override.ErrorMessage,
			Meta:      errMeta,
			Failure:   failure,
			Timestamp: time.Now(),
		})
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnComplete,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Timestamp: time.Now(),
			TurnComplete: &provider.WireTurnCompleteMeta{
				UsageScope:   s.usageScope(),
				StopReason:   "error",
				Usage:        totalUsage,
				ModelUsage:   buildModelUsage(model, totalUsage),
				OwnUsage:     totalUsage,
				ErrorMessage: override.ErrorMessage,
			},
		})
		return
	}

	meta := &provider.WireTurnCompleteMeta{
		UsageScope: s.usageScope(),
		StopReason: stopReason,
		Usage:      totalUsage,
		ModelUsage: buildModelUsage(model, totalUsage),
		OwnUsage:   totalUsage,
		Aborted:    stopReason == "cancelled",
	}
	if stopReason == "refusal" {
		meta.ErrorMessage = "provider refused the turn"
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Content:   meta.ErrorMessage,
			Failure:   &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryTurn, Code: "refusal"},
			Timestamp: time.Now(),
		})
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:             provider.EventTurnComplete,
		ThreadID:         s.threadID,
		TurnID:           turnID,
		Timestamp:        time.Now(),
		TurnComplete:     meta,
		StructuredOutput: structuredOutput,
	})
}

func (s *Session) usageScope() string {
	return fmt.Sprintf("acp:%d:%s", s.PID(), s.SessionID())
}

func buildModelUsage(model string, total *provider.TokenUsage) []provider.ModelTokenUsage {
	if total == nil || total.IsZero() {
		return nil
	}
	if model == "" {
		model = "unknown"
	}
	return []provider.ModelTokenUsage{{
		Model:      model,
		TokenUsage: *total,
	}}
}

// Interrupt sends the ACP session/cancel notification, drains pending approvals,
// and waits briefly for in-flight prompts to settle so a subsequent resend
// after Stop does not race into the cancelling turn.
func (s *Session) Interrupt(ctx context.Context) error {
	s.closeActiveContentBlock()
	s.questionActionMu.Lock()
	s.mu.Lock()
	sessionID := s.sessionID
	heldForQuestion := s.turnOpen && s.inFlightPrompts == 0 && len(s.pendingToolQuestions) > 0
	spontaneousOnly := s.spontaneousTurnOpen && s.inFlightPrompts == 0 && !heldForQuestion
	hasInFlight := s.inFlightPrompts > 0
	turnID := s.activeTurnID
	model := s.model
	s.armedConfirm = nil
	var waitCh chan struct{}
	if hasInFlight {
		s.cancelling = true
		if s.inFlightZeroCh == nil {
			s.inFlightZeroCh = make(chan struct{})
		}
		waitCh = s.inFlightZeroCh
	}
	var totalUsage *provider.TokenUsage
	if heldForQuestion || spontaneousOnly {
		if s.promptUsageSeen {
			u := s.promptUsage
			totalUsage = &u
		}
		s.promptUsage = provider.TokenUsage{}
		s.promptUsageSeen = false
		s.turnOpen = false
		s.spontaneousTurnOpen = false
		s.turnHasOutputSchema = false
		s.turnLastAssistantText.Reset()
		s.turnBeforeToken = nil
		s.turnBeforeSet = false
		s.lastPromptCompact = false
	}
	s.mu.Unlock()
	if sessionID == "" {
		s.questionActionMu.Unlock()
		return nil
	}
	if heldForQuestion {
		s.resolvePendingToolQuestions("declined", nil)
		s.drainPendingApprovals("lost", false)
		s.questionActionMu.Unlock()
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnComplete,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Timestamp: time.Now(),
			TurnComplete: &provider.WireTurnCompleteMeta{
				UsageScope: s.usageScope(),
				StopReason: "cancelled",
				Usage:      totalUsage,
				ModelUsage: buildModelUsage(model, totalUsage),
				OwnUsage:   totalUsage,
				Aborted:    true,
			},
		})
	} else if spontaneousOnly {
		s.drainPendingApprovals("lost", false)
		s.questionActionMu.Unlock()
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnComplete,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Timestamp: time.Now(),
			TurnComplete: &provider.WireTurnCompleteMeta{
				UsageScope: s.usageScope(),
				StopReason: "cancelled",
				Usage:      totalUsage,
				ModelUsage: buildModelUsage(model, totalUsage),
				OwnUsage:   totalUsage,
				Aborted:    true,
			},
		})
	} else {
		s.drainPendingApprovals("lost", false)
		s.questionActionMu.Unlock()
	}
	writeErr := s.conn.writeNotification("session/cancel", SessionCancelParams{
		SessionID: sessionID,
	})
	if writeErr != nil {
		s.mu.Lock()
		s.cancelling = false
		s.mu.Unlock()
		return writeErr
	}
	if waitCh != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		timer := time.NewTimer(interruptSettleTimeout)
		defer timer.Stop()
		select {
		case <-waitCh:
		case <-s.sessionCtx.Done():
		case <-ctx.Done():
		case <-timer.C:
			s.mu.Lock()
			s.cancelling = false
			s.mu.Unlock()
		}
	}
	return nil
}

// RespondToApproval forwards the user's decision for a pending
// session/request_permission callback back to the ACP server.
func (s *Session) RespondToApproval(_ context.Context, resp provider.ApprovalResponse) error {
	s.mu.Lock()
	pending, ok := s.pendingPerms[resp.RequestID]
	sessionID := s.sessionID
	s.mu.Unlock()
	if !ok {
		return provider.ErrStaleInteractiveRequest
	}

	decision := resp.Decision
	var result RequestPermissionResult
	if decision == "cancel" {
		result = RequestPermissionResult{
			Outcome: PermissionOutcome{Outcome: "cancelled"},
		}
	} else if optionID := pending.optionByKey[decision]; optionID != "" {
		result = RequestPermissionResult{
			Outcome: PermissionOutcome{
				Outcome:  "selected",
				OptionID: optionID,
			},
		}
	} else {
		result = RequestPermissionResult{
			Outcome: PermissionOutcome{Outcome: "cancelled"},
		}
	}
	if !s.approvals.Claim(resp.RequestID, provider.EventApprovalResolved) {
		return provider.ErrStaleInteractiveRequest
	}
	if err := s.conn.writeResponse(pending.rpcID, result); err != nil {
		// Re-track so the caller can retry after a transient write failure.
		s.approvals.Track(resp.RequestID, provider.EventApprovalResolved, nil)
		return err
	}
	s.mu.Lock()
	delete(s.pendingPerms, resp.RequestID)
	s.mu.Unlock()
	if decision == "cancel" && sessionID != "" {
		_ = s.conn.writeNotification("session/cancel", SessionCancelParams{SessionID: sessionID})
	}
	return nil
}

// RespondToUserInput answers either an interaction_* permission question (via
// JSON-RPC permission response) or a pending ask_question tool (via text
// session/prompt).
func (s *Session) RespondToUserInput(ctx context.Context, resp provider.UserInputResponse) error {
	decision, err := provider.NormalizeUserInputDecision(resp.Decision)
	if err != nil {
		return err
	}

	s.questionActionMu.Lock()
	s.mu.Lock()
	pendingPermPeek, isPermPeek := s.pendingPerms[resp.RequestID]
	pendingQPeek, isToolQPeek := s.pendingToolQuestions[resp.RequestID]
	if isToolQPeek && !isPermPeek {
		if pendingQPeek.answering {
			s.mu.Unlock()
			s.questionActionMu.Unlock()
			return provider.ErrStaleInteractiveRequest
		}
		pendingQPeek.answering = true
		s.pendingToolQuestions[resp.RequestID] = pendingQPeek
	}
	s.mu.Unlock()
	if !isToolQPeek || isPermPeek {
		s.questionActionMu.Unlock()
	}

	if isPermPeek {
		var result RequestPermissionResult
		if decision == "decline" {
			result = RequestPermissionResult{Outcome: PermissionOutcome{Outcome: "cancelled"}}
		} else {
			optionID := resolveUserInputOptionID(resp.Answers, pendingPermPeek.optionByKey)
			if optionID != "" {
				result = RequestPermissionResult{
					Outcome: PermissionOutcome{Outcome: "selected", OptionID: optionID},
				}
			} else {
				result = RequestPermissionResult{Outcome: PermissionOutcome{Outcome: "cancelled"}}
			}
		}
		if !s.approvals.Claim(resp.RequestID, provider.EventUserInputResolved) {
			return provider.ErrStaleInteractiveRequest
		}
		if err := s.conn.writeResponse(pendingPermPeek.rpcID, result); err != nil {
			s.approvals.Track(resp.RequestID, provider.EventUserInputResolved, nil)
			return err
		}
		s.mu.Lock()
		delete(s.pendingPerms, resp.RequestID)
		s.mu.Unlock()
		return nil
	}

	if !isToolQPeek {
		return provider.ErrStaleInteractiveRequest
	}
	defer s.questionActionMu.Unlock()

	replyText := formatToolQuestionAnswer(pendingQPeek.questions, resp.Answers, decision)
	if decision != "decline" && strings.TrimSpace(replyText) == "" {
		s.mu.Lock()
		if cur, ok := s.pendingToolQuestions[resp.RequestID]; ok {
			cur.answering = false
			s.pendingToolQuestions[resp.RequestID] = cur
		}
		s.mu.Unlock()
		return errors.New("acp: ask_question response requires at least one answer")
	}

	// Claim BEFORE dispatching the follow-up prompt so a concurrent drain
	// cannot invalidate our claim after we already wrote to the wire.
	if !s.approvals.Claim(resp.RequestID, provider.EventUserInputResolved) {
		s.mu.Lock()
		if cur, ok := s.pendingToolQuestions[resp.RequestID]; ok {
			cur.answering = false
			s.pendingToolQuestions[resp.RequestID] = cur
		}
		s.mu.Unlock()
		return provider.ErrStaleInteractiveRequest
	}

	var (
		followUpWait   func(context.Context) (json.RawMessage, error)
		followUpSettle func(rawLine []byte)
		followUpBefore any
	)
	if replyText != "" {
		s.mu.Lock()
		sessionID := s.sessionID
		s.mu.Unlock()
		if sessionID != "" {
			followUpBefore = s.flavor.BeforePrompt(ctx, sessionID)
		}
		s.mu.Lock()
		s.inFlightPrompts++
		s.wireInFlightPrompts++
		s.armedConfirm = nil
		if !s.turnBeforeSet {
			s.turnBeforeToken = followUpBefore
			s.turnBeforeSet = true
		}
		s.mu.Unlock()
		followUpSettle = s.makeWirePromptSettler(nil)
		followUpWait, err = s.startPromptRequest(sessionID, []ContentBlock{{Type: "text", Text: replyText}}, followUpSettle)
		if err != nil {
			followUpSettle(nil)
			s.mu.Lock()
			if s.inFlightPrompts > 0 {
				s.inFlightPrompts--
			}
			if s.inFlightPrompts == 0 {
				s.signalInFlightZeroLocked()
			}
			if cur, ok := s.pendingToolQuestions[resp.RequestID]; ok {
				cur.answering = false
				s.pendingToolQuestions[resp.RequestID] = cur
			}
			s.mu.Unlock()
			s.approvals.Track(resp.RequestID, provider.EventUserInputResolved, pendingQPeek.questions)
			return err
		}
	}

	var openedTurnID string
	s.mu.Lock()
	pendingQ, isToolQ := s.pendingToolQuestions[resp.RequestID]
	turnID := s.activeTurnID
	if isToolQ {
		delete(s.pendingToolQuestions, resp.RequestID)
		delete(s.startedTools, pendingQ.toolCallID)
		if followUpWait != nil && !s.turnOpen {
			s.turnSeq++
			s.activeTurnID = s.nextTurnID("turn")
			s.turnOpen = true
			s.spontaneousTurnOpen = false
			s.promptUsage = provider.TokenUsage{}
			s.promptUsageSeen = false
			turnID = s.activeTurnID
			openedTurnID = turnID
		}
	}
	s.mu.Unlock()

	if openedTurnID != "" {
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  s.threadID,
			TurnID:    openedTurnID,
			Timestamp: time.Now(),
		})
	}

	if isToolQ {
		itemStatus := "completed"
		if decision == "decline" {
			itemStatus = "declined"
		}
		completeMetaFields := map[string]any{
			"toolName":    "AskUserQuestion",
			"item_status": itemStatus,
			"answers":     resp.Answers,
		}
		if followUpWait != nil {
			completeMetaFields["prompt_dispatched"] = true
		}
		completeMeta, _ := json.Marshal(completeMetaFields)
		s.emitEvent(provider.ProviderEvent{
			Kind:           provider.EventToolComplete,
			ThreadID:       s.threadID,
			TurnID:         turnID,
			ItemID:         pendingQ.toolCallID,
			ItemType:       "AskUserQuestion",
			Content:        replyText,
			ContentPresent: replyText != "",
			Meta:           completeMeta,
			Timestamp:      time.Now(),
		})
	}
	if followUpWait != nil {
		s.promptWG.Add(1)
		go func() {
			defer s.promptWG.Done()
			rawResp, waitErr := followUpWait(s.sessionCtx)
			if followUpSettle != nil {
				followUpSettle(nil)
			}
			s.finishPrompt(rawResp, waitErr, followUpBefore, nil)
		}()
	}
	return nil
}

// Close detaches this session from its Conn, sending session/close on it.
func (s *Session) Close() error {
	s.closing.Store(true)
	if s.sessionCancel != nil {
		s.sessionCancel()
	}
	s.stopChunkTimer()
	s.drainPendingApprovals("lost", true)

	var err error
	s.mu.Lock()
	sessionID := s.sessionID
	inFlight := s.inFlightPrompts > 0
	s.mu.Unlock()

	connAlive := true
	select {
	case <-s.conn.Done():
		connAlive = false
	default:
	}

	if connAlive && sessionID != "" {
		if inFlight {
			_ = s.conn.writeNotification("session/cancel", SessionCancelParams{SessionID: sessionID})
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), defaultCloseTimeout)
		_, closeErr := s.conn.sendRequest(closeCtx, "session/close", SessionCloseParams{SessionID: sessionID})
		cancel()
		if closeErr != nil && !IsMethodNotFound(closeErr) && !IsSessionNotFound(closeErr) {
			err = closeErr
		}
	}

	if sessionID != "" {
		s.conn.unregisterSession(sessionID, s)
	}
	s.promptWG.Wait()
	s.subagentWG.Wait()
	s.emitTeardownOnce(nil, true)
	s.finishReadDone()
	s.releasePoolRef()
	return err
}

func (s *Session) onConnClosed(exitMeta json.RawMessage, connClosing bool) {
	wasClosing := s.closing.Swap(true)
	if s.sessionCancel != nil {
		s.sessionCancel()
	}
	s.mu.Lock()
	s.cancelling = false
	s.signalInFlightZeroLocked()
	s.mu.Unlock()
	s.stopChunkTimer()
	s.drainPendingApprovals("lost", true)
	s.emitTeardownOnce(exitMeta, connClosing || wasClosing)
	s.finishReadDone()
}

func (s *Session) releasePoolRef() {
	if s.releaseConn != nil {
		s.releaseConnOnce.Do(s.releaseConn)
	}
}

func (s *Session) finishReadDone() {
	s.readDoneOnce.Do(func() {
		close(s.readDone)
	})
}

func (s *Session) emitTeardownOnce(exitMeta json.RawMessage, intentional bool) {
	if !s.teardownEmitted.CompareAndSwap(false, true) {
		return
	}
	provider.EmitTeardownStatusWithMeta(s.emitEvent, s.threadID, exitMeta, intentional)
}

func (s *Session) drainPendingApprovals(decision string, closeRegistry bool) {
	released := s.approvals.Drain(closeRegistry)
	s.mu.Lock()
	perms := s.pendingPerms
	s.pendingPerms = make(map[string]pendingPermission)
	s.pendingToolQuestions = make(map[string]pendingToolQuestion)
	s.mu.Unlock()

	for _, r := range released {
		if p, ok := perms[r.RequestID]; ok {
			_ = s.conn.writeResponse(p.rpcID, RequestPermissionResult{
				Outcome: PermissionOutcome{Outcome: "cancelled"},
			})
		}
		s.emitEvent(provider.ProviderEvent{
			Kind:      r.ResolveKind,
			ThreadID:  s.threadID,
			ItemID:    r.RequestID,
			Meta:      r.Meta(decision),
			Timestamp: time.Now(),
		})
	}
}

func (s *Session) emitResolvedDetachedToolQuestions(qs map[string]pendingToolQuestion, turnID, decision string, answers map[string]provider.UserInputAnswer) {
	if len(qs) == 0 {
		return
	}
	itemStatus := "completed"
	if decision == "declined" || decision == "decline" {
		itemStatus = "declined"
	}
	for id := range qs {
		if !s.approvals.Claim(id, provider.EventUserInputResolved) {
			continue
		}
		now := time.Now()
		meta, _ := json.Marshal(map[string]any{
			"requestId": id,
			"decision":  decision,
			"answers":   answers,
		})
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventUserInputResolved,
			ThreadID:  s.threadID,
			ItemID:    id,
			Meta:      meta,
			Timestamp: now,
		})
		completeMeta, _ := json.Marshal(map[string]any{
			"toolName":    "AskUserQuestion",
			"item_status": itemStatus,
			"answers":     answers,
		})
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventToolComplete,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			ItemID:    id,
			ItemType:  "AskUserQuestion",
			Meta:      completeMeta,
			Timestamp: now,
		})
	}
}

func (s *Session) snapshotPendingToolQuestionIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingToolQuestions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.pendingToolQuestions))
	for id := range s.pendingToolQuestions {
		ids = append(ids, id)
	}
	return ids
}

func (s *Session) resolvePendingToolQuestions(decision string, answers map[string]provider.UserInputAnswer) {
	s.resolveSpecificToolQuestions(s.snapshotPendingToolQuestionIDs(), decision, answers)
}

func (s *Session) resolveSpecificToolQuestions(targetIDs []string, decision string, answers map[string]provider.UserInputAnswer) {
	if len(targetIDs) == 0 {
		return
	}
	s.mu.Lock()
	if len(s.pendingToolQuestions) == 0 {
		s.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(targetIDs))
	turnID := s.activeTurnID
	for _, id := range targetIDs {
		if _, ok := s.pendingToolQuestions[id]; !ok {
			continue
		}
		ids = append(ids, id)
		delete(s.pendingToolQuestions, id)
		delete(s.startedTools, id)
	}
	s.mu.Unlock()

	itemStatus := "completed"
	if decision == "declined" || decision == "decline" {
		itemStatus = "declined"
	}
	for _, id := range ids {
		if !s.approvals.Claim(id, provider.EventUserInputResolved) {
			continue
		}
		now := time.Now()
		meta, _ := json.Marshal(map[string]any{
			"requestId": id,
			"decision":  decision,
			"answers":   answers,
		})
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventUserInputResolved,
			ThreadID:  s.threadID,
			ItemID:    id,
			Meta:      meta,
			Timestamp: now,
		})
		completeMeta, _ := json.Marshal(map[string]any{
			"toolName":    "AskUserQuestion",
			"item_status": itemStatus,
			"answers":     answers,
		})
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventToolComplete,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			ItemID:    id,
			ItemType:  "AskUserQuestion",
			Meta:      completeMeta,
			Timestamp: now,
		})
	}
}

func (s *Session) handleUpdate(u SessionUpdate) {
	switch u.SessionUpdate {
	case "agent_thought_chunk":
		block, ok := u.decodeContentBlock()
		if !ok || block.Text == "" {
			return
		}
		s.confirmArmedPrompt()
		s.ensureSpontaneousTurnOpen()
		s.bufferChunk(provider.EventThinking, block.Text)

	case "agent_message_chunk":
		block, ok := u.decodeContentBlock()
		if !ok || block.Text == "" {
			return
		}
		s.confirmArmedPrompt()
		s.ensureSpontaneousTurnOpen()
		s.mu.Lock()
		if s.turnHasOutputSchema {
			s.turnLastAssistantText.WriteString(block.Text)
		}
		s.mu.Unlock()
		s.bufferChunk(provider.EventTextDelta, block.Text)

	case "tool_call":
		tc := u.toToolCallUpdate()
		parentID, isChild, drop := s.resolveChildParentToolUseID(tc.ToolCallID)
		if drop {
			return
		}
		if !isChild {
			s.confirmArmedPrompt()
			s.closeActiveContentBlock()
			s.ensureSpontaneousTurnOpen()
			s.mu.Lock()
			if s.turnHasOutputSchema {
				s.turnLastAssistantText.Reset()
			}
			s.mu.Unlock()
		}
		s.handleToolCallStart(&tc, parentID)

	case "tool_call_update":
		tc := u.toToolCallUpdate()
		parentID, isChild, drop := s.resolveChildParentToolUseID(tc.ToolCallID)
		if drop {
			return
		}
		if !isChild {
			s.flushChunks()
		}
		switch tc.Status {
		case "completed", "failed", "cancelled":
			s.handleToolCallComplete(&tc, parentID)
		case "pending":
			if !isChild {
				s.closeActiveContentBlock()
				s.ensureSpontaneousTurnOpen()
				s.handleToolCallPending(&tc)
			}
		case "in_progress":
			s.handleToolCallInProgress(&tc, parentID)
		}

	case "plan":
		s.confirmArmedPrompt()
		s.closeActiveContentBlock()
		s.handlePlanUpdate(u.Entries)

	case "compaction_update":
		s.closeActiveContentBlock()
		s.mu.Lock()
		turnID := s.activeTurnID
		preTokens := s.lastUsedTokens
		s.mu.Unlock()
		meta, _ := json.Marshal(map[string]any{
			"trigger":    "auto",
			"pre_tokens": preTokens,
		})
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventCompactBoundary,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Content:   "Context compacted",
			Meta:      meta,
			Timestamp: time.Now(),
		})

	case "usage_update":
		s.closeActiveContentBlock()
		s.handleUsageUpdate(u.Used, u.Size)

	case "available_commands_update":
		s.handleAvailableCommands(u.AvailableCommands)

	case "config_option_update":
		s.mu.Lock()
		if len(u.ConfigOptions) > 0 {
			s.configOptions = append([]ConfigOption(nil), u.ConfigOptions...)
			if m := currentConfigValue(s.configOptions, "model"); m != "" {
				s.model = m
			}
			s.reasoningEffort = currentConfigValue(s.configOptions, "thought_level")
		}
		s.mu.Unlock()

	case "current_mode_update":
		s.handleCurrentModeUpdate(u.CurrentModeID)

	case "session_info_update":
		if title := strings.TrimSpace(u.Title); title != "" {
			s.emitEvent(provider.ProviderEvent{
				Kind:      provider.EventThreadRenamed,
				ThreadID:  s.threadID,
				Content:   title,
				Timestamp: time.Now(),
			})
		}
	}
}

func (s *Session) nextTurnID(kind string) string {
	if s.instanceID != "" {
		return fmt.Sprintf("acp-%s-%s-%d", kind, s.instanceID, s.turnSeq)
	}
	return fmt.Sprintf("acp-%s-%d", kind, s.turnSeq)
}

func newSessionInstanceID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func (s *Session) ensureSpontaneousTurnOpen() {
	var openTurnID string
	s.mu.Lock()
	if !s.turnOpen && s.inFlightPrompts == 0 {
		s.turnSeq++
		s.activeTurnID = s.nextTurnID("wake")
		s.turnOpen = true
		s.spontaneousTurnOpen = true
		s.promptUsage = provider.TokenUsage{}
		s.promptUsageSeen = false
		openTurnID = s.activeTurnID
	}
	s.mu.Unlock()
	if openTurnID != "" {
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  s.threadID,
			TurnID:    openTurnID,
			Timestamp: time.Now(),
		})
	}
}

func (s *Session) bufferChunk(kind provider.EventKind, text string) {
	var flushKind provider.EventKind
	var flushText string
	var stopPrevKind provider.EventKind
	var turnID string

	s.mu.Lock()
	turnID = s.activeTurnID
	if s.activeBlockKind != "" && s.activeBlockKind != kind {
		stopPrevKind = s.activeBlockKind
		if s.chunk.buf.Len() > 0 {
			flushKind = s.chunk.kind
			flushText = s.chunk.buf.String()
			s.chunk.buf.Reset()
			if s.chunk.timer != nil {
				s.chunk.timer.Stop()
				s.chunk.timer = nil
			}
		}
	}
	s.activeBlockKind = kind
	s.chunk.kind = kind
	s.chunk.buf.WriteString(text)
	if s.chunk.buf.Len() >= chunkCoalesceBytes {
		curText := s.chunk.buf.String()
		s.chunk.buf.Reset()
		if s.chunk.timer != nil {
			s.chunk.timer.Stop()
			s.chunk.timer = nil
		}
		s.mu.Unlock()
		if flushText != "" {
			s.emitChunk(flushKind, turnID, flushText)
		}
		if stopPrevKind != "" {
			s.emitContentBlockStop(stopPrevKind, turnID)
		}
		s.emitChunk(kind, turnID, curText)
		return
	}
	if s.chunk.timer == nil {
		s.chunk.timer = time.AfterFunc(chunkCoalesceWindow, s.flushChunks)
	}
	s.mu.Unlock()

	if flushText != "" {
		s.emitChunk(flushKind, turnID, flushText)
	}
	if stopPrevKind != "" {
		s.emitContentBlockStop(stopPrevKind, turnID)
	}
}

func (s *Session) emitContentBlockStop(kind provider.EventKind, turnID string) {
	if s.closing.Load() || kind == "" {
		return
	}
	blockType := "text"
	if kind == provider.EventThinking {
		blockType = "thinking"
	}
	meta, _ := json.Marshal(map[string]any{"blockType": blockType})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventContentBlockStop,
		ThreadID:  s.threadID,
		TurnID:    turnID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (s *Session) closeActiveContentBlock() {
	s.flushChunks()
	s.mu.Lock()
	kind := s.activeBlockKind
	turnID := s.activeTurnID
	s.activeBlockKind = ""
	s.mu.Unlock()
	if kind != "" {
		s.emitContentBlockStop(kind, turnID)
	}
}

func (s *Session) flushChunks() {
	s.mu.Lock()
	if s.chunk.timer != nil {
		s.chunk.timer.Stop()
		s.chunk.timer = nil
	}
	if s.chunk.buf.Len() == 0 {
		s.mu.Unlock()
		return
	}
	kind := s.chunk.kind
	text := s.chunk.buf.String()
	turnID := s.activeTurnID
	s.chunk.buf.Reset()
	s.mu.Unlock()

	if s.closing.Load() {
		return
	}
	s.emitChunk(kind, turnID, text)
}

func (s *Session) stopChunkTimer() {
	s.mu.Lock()
	if s.chunk.timer != nil {
		s.chunk.timer.Stop()
		s.chunk.timer = nil
	}
	s.mu.Unlock()
}

func (s *Session) emitChunk(kind provider.EventKind, turnID, text string) {
	if text == "" {
		return
	}
	s.emitEvent(provider.ProviderEvent{
		Kind:      kind,
		ThreadID:  s.threadID,
		TurnID:    turnID,
		Content:   text,
		Timestamp: time.Now(),
	})
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func (s *Session) registerChildLaunch(childConvID, launchToolCallID string) {
	if childConvID == "" || launchToolCallID == "" {
		return
	}
	s.mu.Lock()
	s.childLaunchByConvID[childConvID] = launchToolCallID
	s.mu.Unlock()
}

// resolveChildParentToolUseID detects subagent tool calls forwarded by an ACP
// server onto the parent stream with IDs shaped "<childConversationId>:<callId>".
// When correlated to an invoke_subagent launch, it returns that launch's
// ToolCallID as parentID so the call nests inside the subagent's pane. When a
// foreign child UUID cannot be correlated, drop is true so child work never
// leaks into the main timeline.
func (s *Session) resolveChildParentToolUseID(toolCallID string) (parentID string, isChild bool, drop bool) {
	colon := strings.IndexByte(toolCallID, ':')
	if colon <= 0 {
		return "", false, false
	}
	prefix := toolCallID[:colon]
	if !looksLikeUUID(prefix) {
		return "", false, false
	}
	s.mu.Lock()
	sessionID := s.sessionID
	cached := s.childLaunchByConvID[prefix]
	s.mu.Unlock()
	if prefix == sessionID {
		return "", false, false
	}
	if cached != "" {
		return cached, true, false
	}
	if launchID, ok := s.flavor.ResolveSubagentLaunch(s.sessionCtx, sessionID, prefix); ok && launchID != "" {
		s.registerChildLaunch(prefix, launchID)
		return launchID, true, false
	}
	return "", true, true
}

func (s *Session) handleToolCallStart(tc *ToolCallUpdate, parentToolUseID string) {
	if tc.ToolCallID == "" {
		return
	}
	norm := s.flavor.NormalizeToolStart(tc)

	s.mu.Lock()
	sessionID := s.sessionID
	turnID := s.activeTurnID
	parentModel := s.model
	parentEffort := s.reasoningEffort
	if parentToolUseID == "" {
		if norm.SubagentLaunch && s.flavor.CanTrackSubagent(sessionID) {
			norm.IsBackground = true
			launchCallID := tc.ToolCallID
			s.trackedSubagents[tc.ToolCallID] = SubagentLaunchContext{
				SessionID:    sessionID,
				ThreadID:     s.threadID,
				TurnID:       turnID,
				ToolCallID:   launchCallID,
				RawInput:     append(json.RawMessage(nil), tc.EffectiveInput()...),
				Started:      norm,
				ParentModel:  parentModel,
				ParentEffort: parentEffort,
				Emit:         s.emitEvent,
				OnChildDiscovered: func(childConvID string) {
					s.registerChildLaunch(childConvID, launchCallID)
				},
			}
		}
		s.startedTools[tc.ToolCallID] = norm
	} else {
		s.childStartedTools[tc.ToolCallID] = norm
	}
	s.mu.Unlock()

	meta := buildToolStartMeta(norm)
	s.emitEvent(provider.ProviderEvent{
		Kind:            provider.EventToolStart,
		ThreadID:        s.threadID,
		TurnID:          turnID,
		ItemID:          tc.ToolCallID,
		ItemType:        norm.ItemType,
		ParentToolUseID: parentToolUseID,
		Meta:            meta,
		Timestamp:       time.Now(),
	})

	if parentToolUseID == "" && tc.Status == "pending" {
		s.handleToolCallPending(tc)
	}
}

func (s *Session) handleToolCallInProgress(tc *ToolCallUpdate, parentToolUseID string) {
	if tc.ToolCallID == "" {
		return
	}
	s.mu.Lock()
	var started NormalizedToolStart
	if parentToolUseID == "" {
		started = s.startedTools[tc.ToolCallID]
	} else {
		started = s.childStartedTools[tc.ToolCallID]
	}
	turnID := s.activeTurnID
	s.mu.Unlock()

	if tc.Kind != "execute" && started.ItemType != "command_execution" {
		return
	}
	norm := s.flavor.NormalizeToolComplete(tc, started)
	if parentToolUseID == "" && norm.IsBackground {
		var emitBgMeta bool
		s.mu.Lock()
		cur, exists := s.startedTools[tc.ToolCallID]
		if exists {
			prevOut, _ := cur.ExtraMeta["output_file"].(string)
			prevTask, _ := cur.ExtraMeta["task_id"].(string)
			if !cur.IsBackground || (norm.OutputFile != "" && prevOut == "") || (norm.BackgroundTaskID != "" && prevTask == "") {
				cur.IsBackground = true
				if cur.ExtraMeta == nil {
					cur.ExtraMeta = map[string]any{}
				}
				if norm.OutputFile != "" && prevOut == "" {
					cur.ExtraMeta["output_file"] = norm.OutputFile
				}
				if norm.BackgroundTaskID != "" && prevTask == "" {
					cur.ExtraMeta["task_id"] = norm.BackgroundTaskID
				}
				s.startedTools[tc.ToolCallID] = cur
				emitBgMeta = true
			}
		}
		s.mu.Unlock()
		if emitBgMeta {
			bgFields := map[string]any{
				"meta_update_only": true,
				"is_background":    true,
			}
			if norm.BackgroundTaskID != "" {
				bgFields["task_id"] = norm.BackgroundTaskID
			}
			if norm.OutputFile != "" {
				bgFields["output_file"] = norm.OutputFile
			}
			bgMeta, _ := json.Marshal(bgFields)
			// Omit ItemType on meta_update_only so triage preserves the
			// existing Bash command summary instead of rewriting it.
			s.emitEvent(provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ThreadID:  s.threadID,
				TurnID:    turnID,
				ItemID:    tc.ToolCallID,
				Meta:      bgMeta,
				Timestamp: time.Now(),
			})
		}
	}
	if norm.CommandOut == "" {
		return
	}
	cmdMeta, _ := json.Marshal(map[string]any{
		"command":  norm.CommandLine,
		"toolName": norm.ToolName,
	})
	s.emitEvent(provider.ProviderEvent{
		Kind:            provider.EventCommandOutput,
		ThreadID:        s.threadID,
		TurnID:          turnID,
		ItemID:          tc.ToolCallID,
		ItemType:        "command_execution",
		ParentToolUseID: parentToolUseID,
		Content:         norm.CommandOut,
		Meta:            cmdMeta,
		Replace:         true,
		Timestamp:       time.Now(),
	})
}

func (s *Session) handleToolCallPending(tc *ToolCallUpdate) {
	if tc.ToolCallID == "" {
		return
	}
	effectiveTC := *tc
	if effectiveTC.Name == "" || len(effectiveTC.EffectiveInput()) == 0 {
		s.mu.Lock()
		started := s.startedTools[tc.ToolCallID]
		s.mu.Unlock()
		if effectiveTC.Name == "" {
			effectiveTC.Name = started.ToolName
		}
		if len(effectiveTC.EffectiveInput()) == 0 && len(started.Input) > 0 {
			effectiveTC.RawInput = started.Input
		}
	}
	questions, ok := s.flavor.ExtractPendingQuestion(&effectiveTC)
	if !ok || len(questions) == 0 {
		return
	}
	questions = provider.NormalizeUserInputQuestions(questions)

	s.mu.Lock()
	if _, already := s.pendingToolQuestions[tc.ToolCallID]; already {
		s.mu.Unlock()
		return
	}
	turnID := s.activeTurnID
	s.pendingToolQuestions[tc.ToolCallID] = pendingToolQuestion{
		toolCallID: tc.ToolCallID,
		questions:  questions,
	}
	s.mu.Unlock()

	s.approvals.Track(tc.ToolCallID, provider.EventUserInputResolved, questions)
	req := provider.UserInputRequest{
		RequestID: tc.ToolCallID,
		ThreadID:  s.threadID,
		TurnID:    turnID,
		ToolUseID: tc.ToolCallID,
		ToolName:  "ask_question",
		Title:     questions[0].Question,
		Questions: questions,
	}
	meta, _ := json.Marshal(req)
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventUserInputRequest,
		ThreadID:  s.threadID,
		TurnID:    turnID,
		ItemID:    tc.ToolCallID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (s *Session) handleToolCallComplete(tc *ToolCallUpdate, parentToolUseID string) {
	if tc.ToolCallID == "" {
		return
	}
	s.mu.Lock()
	var started NormalizedToolStart
	var trackedLaunch SubagentLaunchContext
	var wasTracked bool
	var watchSubagent bool
	if parentToolUseID == "" {
		started = s.startedTools[tc.ToolCallID]
		delete(s.startedTools, tc.ToolCallID)
		trackedLaunch, wasTracked = s.trackedSubagents[tc.ToolCallID]
		delete(s.trackedSubagents, tc.ToolCallID)
		watchSubagent = wasTracked && tc.Status == "completed" && !s.closing.Load()
		if watchSubagent {
			s.activeSubagentWatchers++
		}
	} else {
		started = s.childStartedTools[tc.ToolCallID]
		delete(s.childStartedTools, tc.ToolCallID)
	}
	turnID := s.activeTurnID
	s.mu.Unlock()

	norm := s.flavor.NormalizeToolComplete(tc, started)
	if watchSubagent {
		if norm.ExtraMeta == nil {
			norm.ExtraMeta = map[string]any{}
		}
		norm.ExtraMeta["is_background"] = true
	}
	now := time.Now()
	for _, evt := range buildToolCompleteEvents(s.threadID, turnID, tc.ToolCallID, norm, now) {
		evt.ParentToolUseID = parentToolUseID
		s.emitEvent(evt)
	}
	if watchSubagent {
		s.subagentWG.Add(1)
		go func() {
			defer func() {
				s.mu.Lock()
				if s.activeSubagentWatchers > 0 {
					s.activeSubagentWatchers--
				}
				s.mu.Unlock()
				s.subagentWG.Done()
			}()
			s.flavor.WatchSubagent(s.sessionCtx, trackedLaunch)
		}()
	}
}

// BuildToolStartMeta encodes normalized tool-start metadata for EventToolStart.
func BuildToolStartMeta(norm NormalizedToolStart) json.RawMessage {
	return buildToolStartMeta(norm)
}

// BuildToolCompleteEvents constructs the companion heavy-payload events and
// terminal EventToolComplete for a normalized tool completion.
func BuildToolCompleteEvents(threadID, turnID, toolCallID string, norm NormalizedToolComplete, now time.Time) []provider.ProviderEvent {
	return buildToolCompleteEvents(threadID, turnID, toolCallID, norm, now)
}

func buildToolStartMeta(norm NormalizedToolStart) json.RawMessage {
	metaFields := map[string]any{
		"toolName": norm.ToolName,
	}
	if len(norm.Input) > 0 {
		metaFields["input"] = norm.Input
	}
	if norm.SubagentLaunch {
		metaFields[provider.MetaSubagentLaunchKey] = true
	}
	if norm.Description != "" {
		metaFields["description"] = norm.Description
	}
	if norm.SubagentType != "" {
		metaFields["subagent_type"] = norm.SubagentType
	}
	if norm.IsBackground {
		metaFields["is_background"] = true
	}
	for k, v := range norm.ExtraMeta {
		metaFields[k] = v
	}
	meta, _ := json.Marshal(metaFields)
	return meta
}

func buildToolCompleteEvents(threadID, turnID, toolCallID string, norm NormalizedToolComplete, now time.Time) []provider.ProviderEvent {
	var events []provider.ProviderEvent

	// File-change tool rows carry structured item.changes on EventToolComplete
	// so triage.ExtractFileChangeToolResult builds exact ToolInlineDiff previews
	// and insertion/deletion tallies directly on the Edit/Write row. Only emit a
	// standalone EventDiff when the tool isn't already a file_change row with
	// structured changes.
	if norm.DiffPatch != "" && (norm.ItemType != "file_change" || len(norm.DiffChanges) == 0) {
		diffMeta, _ := json.Marshal(map[string]any{
			"filePath": norm.DiffFilePath,
			"toolName": norm.ToolName,
		})
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    toolCallID,
			ItemType:  "file_change",
			Content:   norm.DiffPatch,
			Meta:      diffMeta,
			Replace:   true,
			Timestamp: now,
		})
	}
	if norm.CommandOut != "" {
		cmdMeta := map[string]any{
			"command":  norm.CommandLine,
			"toolName": norm.ToolName,
		}
		if norm.ExitCode != nil {
			cmdMeta["exit_code"] = *norm.ExitCode
			cmdMeta["exitCode"] = *norm.ExitCode
		}
		if norm.DurationMs != nil {
			cmdMeta["durationMs"] = *norm.DurationMs
		}
		encoded, _ := json.Marshal(cmdMeta)
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventCommandOutput,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    toolCallID,
			ItemType:  "command_execution",
			Content:   norm.CommandOut,
			Meta:      encoded,
			Replace:   true,
			Timestamp: now,
		})
	}
	if norm.ProposedPlan != "" {
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventProposedPlan,
			ThreadID:  threadID,
			TurnID:    turnID,
			ItemID:    toolCallID + ":plan",
			ItemType:  "plan",
			Content:   norm.ProposedPlan,
			Timestamp: now,
		})
	}

	metaFields := map[string]any{
		"toolName": norm.ToolName,
		"is_error": norm.IsError,
	}
	if norm.CommandLine != "" {
		metaFields["command"] = norm.CommandLine
	}
	if norm.ExitCode != nil {
		metaFields["exit_code"] = *norm.ExitCode
		metaFields["exitCode"] = *norm.ExitCode
	}
	if norm.DurationMs != nil {
		metaFields["durationMs"] = *norm.DurationMs
	}
	if norm.ItemStatus != "" {
		metaFields["item_status"] = norm.ItemStatus
	}
	if norm.IsBackground {
		metaFields["is_background"] = true
	}
	if norm.BackgroundTaskID != "" {
		metaFields["task_id"] = norm.BackgroundTaskID
	}
	if norm.OutputFile != "" {
		metaFields["output_file"] = norm.OutputFile
	}
	if norm.ItemType == "file_change" && len(norm.DiffChanges) > 0 {
		changes := make([]map[string]any, 0, len(norm.DiffChanges))
		for _, ch := range norm.DiffChanges {
			kindType := ch.Kind
			if kindType == "" {
				kindType = "update"
			}
			changes = append(changes, map[string]any{
				"path": ch.Path,
				"kind": map[string]string{"type": kindType},
				"diff": ch.Diff,
			})
		}
		metaFields["item"] = map[string]any{
			"id":      toolCallID,
			"type":    "file_change",
			"status":  norm.ItemStatus,
			"changes": changes,
		}
	}
	for k, v := range norm.ExtraMeta {
		metaFields[k] = v
	}
	meta, _ := json.Marshal(metaFields)
	events = append(events, provider.ProviderEvent{
		Kind:           provider.EventToolComplete,
		ThreadID:       threadID,
		TurnID:         turnID,
		ItemID:         toolCallID,
		ItemType:       norm.ItemType,
		Content:        norm.Content,
		ContentPresent: norm.Content != "",
		Meta:           meta,
		Timestamp:      now,
	})
	return events
}

func appendOutputSchemaInstruction(content string, schema json.RawMessage) string {
	trimmedSchema := strings.TrimSpace(string(schema))
	if trimmedSchema == "" {
		return content
	}
	instruction := "\n\n<structured-output-schema>\nRespond in your final assistant message with ONLY a single valid JSON object matching this JSON Schema (no markdown fences, no commentary before or after):\n" + trimmedSchema + "\n</structured-output-schema>"
	return content + instruction
}

// extractStructuredJSONObject extracts a single JSON object from assistant text
// (accepting raw JSON, fenced ```json blocks, or a trailing JSON object).
func extractStructuredJSONObject(text string) json.RawMessage {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "```") {
		if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
			body := trimmed[nl+1:]
			if end := strings.LastIndex(body, "```"); end >= 0 {
				trimmed = strings.TrimSpace(body[:end])
			}
		}
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	// Scan candidate JSON objects from the end so prose containing '{' before
	// the trailing JSON object does not invalidate extraction.
	for end := strings.LastIndexByte(trimmed, '}'); end > 0; end = strings.LastIndexByte(trimmed[:end], '}') {
		for start := strings.LastIndexByte(trimmed[:end], '{'); start >= 0; start = strings.LastIndexByte(trimmed[:start], '{') {
			candidate := strings.TrimSpace(trimmed[start : end+1])
			if json.Valid([]byte(candidate)) {
				return json.RawMessage(candidate)
			}
			if start == 0 {
				break
			}
		}
	}
	return nil
}

func (s *Session) handlePlanUpdate(entries []PlanEntry) {
	type planStep struct {
		Step   string `json:"step"`
		Status string `json:"status"`
	}
	steps := make([]planStep, 0, len(entries))
	for _, e := range entries {
		status := e.Status
		if status == "in_progress" {
			status = "inProgress"
		}
		steps = append(steps, planStep{Step: e.Content, Status: status})
	}
	meta, _ := json.Marshal(map[string]any{"plan": steps})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventTodoUpdate,
		ThreadID:  s.threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (s *Session) maybeEmitCompaction(sessionID, turnID, defaultTrigger string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	afterIdx := s.lastCompactionIdx
	preTokens := s.lastUsedTokens
	s.mu.Unlock()

	nextIdx, ev := s.flavor.DetectCompaction(context.Background(), sessionID, afterIdx)
	if ev == nil {
		return
	}
	s.mu.Lock()
	if nextIdx <= s.lastCompactionIdx {
		s.mu.Unlock()
		return
	}
	s.lastCompactionIdx = nextIdx
	s.mu.Unlock()

	trigger := ev.Trigger
	if trigger == "" {
		trigger = defaultTrigger
	}
	if trigger == "" {
		trigger = "auto"
	}
	if ev.PreTokens > 0 {
		preTokens = ev.PreTokens
	}
	metaFields := map[string]any{
		"trigger": trigger,
	}
	if preTokens > 0 {
		metaFields["pre_tokens"] = preTokens
	}
	if ev.Summary != "" {
		metaFields["summary"] = ev.Summary
	}
	meta, _ := json.Marshal(metaFields)
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventCompactBoundary,
		ThreadID:  s.threadID,
		TurnID:    turnID,
		ItemID:    ev.ItemID,
		Content:   "Context compacted",
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (s *Session) handleUsageUpdate(used, size int) {
	s.mu.Lock()
	if size > 0 {
		s.lastContextSize = size
	} else {
		size = s.lastContextSize
	}
	prevUsed := s.lastUsedTokens
	wasCompact := s.lastPromptCompact
	sessionID := s.sessionID
	spontaneous := s.spontaneousTurnOpen && s.inFlightPrompts == 0 &&
		len(s.startedTools) == 0 && len(s.pendingPerms) == 0 && len(s.pendingToolQuestions) == 0
	turnID := s.activeTurnID
	s.mu.Unlock()

	// Detect out-of-band compaction (e.g. agy CHECKPOINT step_type=23) when
	// context occupancy drops sharply or right after an explicit /compact.
	if (used > 0 && prevUsed > 0 && used < prevUsed) || wasCompact {
		trigger := "auto"
		if wasCompact {
			trigger = "manual"
		}
		s.maybeEmitCompaction(sessionID, turnID, trigger)
	}

	if used > 0 {
		s.mu.Lock()
		s.lastUsedTokens = used
		s.mu.Unlock()
	}

	if used > 0 || size > 0 {
		cw := provider.ContextWindow{
			UsedTokens:     used,
			MaxTokens:      size,
			UsedPercentage: provider.ComputeContextPercent(s.flavor.ProviderKind(), used, size),
		}
		meta, _ := json.Marshal(cw)
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventTokenUsage,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}

	// Trailing usage_update closes a spontaneous post-prompt wake turn once no
	// parent tools, permissions, or questions remain open and the flavor reports
	// no queued high-priority wake.
	if spontaneous && !s.flavor.ShouldHoldSpontaneousWakeClose(context.Background(), sessionID) {
		s.mu.Lock()
		if s.spontaneousTurnOpen && s.inFlightPrompts == 0 &&
			len(s.startedTools) == 0 && len(s.pendingPerms) == 0 && len(s.pendingToolQuestions) == 0 {
			s.spontaneousTurnOpen = false
			s.turnOpen = false
			s.promptUsage = provider.TokenUsage{}
			s.promptUsageSeen = false
		} else {
			spontaneous = false
		}
		s.mu.Unlock()
		if spontaneous {
			s.emitEvent(provider.ProviderEvent{
				Kind:      provider.EventTurnComplete,
				ThreadID:  s.threadID,
				TurnID:    turnID,
				Timestamp: time.Now(),
				TurnComplete: &provider.WireTurnCompleteMeta{
					UsageScope: s.usageScope(),
					StopReason: "end_turn",
				},
			})
		}
	}
}

func (s *Session) handleAvailableCommands(commands []AvailableCommand) {
	out := make([]provider.SlashCommand, 0, len(commands))
	for _, cmd := range commands {
		name := strings.TrimPrefix(strings.TrimSpace(cmd.Name), "/")
		if name == "" {
			continue
		}
		hint := ""
		if cmd.Input != nil {
			hint = strings.TrimSpace(cmd.Input.Hint)
		}
		out = append(out, provider.SlashCommand{
			Name:         name,
			Description:  strings.TrimSpace(cmd.Description),
			ArgumentHint: hint,
		})
	}
	meta, _ := json.Marshal(provider.CommandsChangedMeta{Commands: out})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventCommandsChanged,
		ThreadID:  s.threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (s *Session) handleCurrentModeUpdate(modeID string) {
	modeID = strings.TrimSpace(modeID)
	if modeID == "" {
		return
	}
	s.mu.Lock()
	echo := s.lastRequestedModeID == modeID
	if echo {
		s.lastRequestedModeID = ""
	}
	s.currentModeID = modeID
	if !echo {
		switch modeID {
		case "plan":
			s.interactionMode = provider.ModePlan
		case "accept-edits":
			s.interactionMode = provider.ModeChat
			s.runtimeMode = provider.RuntimeAutoAcceptEdits
		case "auto":
			s.interactionMode = provider.ModeChat
			s.runtimeMode = provider.RuntimeAuto
		case "default", "ask":
			if s.interactionMode == provider.ModePlan {
				s.interactionMode = provider.ModeChat
			}
		}
		_, s.permissionPolicy = s.flavor.MapRuntimeMode(s.runtimeMode, s.interactionMode, s.availableModes)
	}
	s.mu.Unlock()
	if echo {
		return
	}
	meta, _ := json.Marshal(provider.PermissionModeChangeMeta{Mode: modeID})
	s.emitEvent(provider.ProviderEvent{
		Kind:      provider.EventPermissionModeChanged,
		ThreadID:  s.threadID,
		Meta:      meta,
		Timestamp: time.Now(),
	})
}

func (s *Session) isToolDisabled(name string) bool {
	if len(s.disabledTools) == 0 {
		return false
	}
	_, disabled := s.disabledTools[strings.ToLower(strings.TrimSpace(name))]
	return disabled
}

func (s *Session) handlePermissionRequest(rpcID json.RawMessage, req RequestPermissionParams) {
	_, isChild, _ := s.resolveChildParentToolUseID(req.ToolCall.ToolCallID)
	if !isChild {
		s.confirmArmedPrompt()
		s.closeActiveContentBlock()
		s.ensureSpontaneousTurnOpen()
	}

	if s.isToolDisabled(req.ToolCall.Name) {
		outcome := PermissionOutcome{Outcome: "cancelled"}
		for _, opt := range req.Options {
			if opt.Kind == "reject_once" || opt.Kind == "reject_always" {
				outcome = PermissionOutcome{Outcome: "selected", OptionID: opt.OptionID}
				break
			}
		}
		_ = s.conn.writeResponse(rpcID, RequestPermissionResult{Outcome: outcome})
		return
	}

	s.mu.Lock()
	policy := s.permissionPolicy
	turnID := s.activeTurnID
	s.mu.Unlock()

	requestID := permissionRequestID(rpcID, req.ToolCall.ToolCallID)
	cls := s.flavor.ClassifyPermission(s.threadID, turnID, requestID, req, policy)

	switch cls.Action {
	case PermissionAutoAllow:
		outcome := PermissionOutcome{Outcome: "cancelled"}
		if cls.OptionID != "" {
			outcome = PermissionOutcome{Outcome: "selected", OptionID: cls.OptionID}
		}
		_ = s.conn.writeResponse(rpcID, RequestPermissionResult{Outcome: outcome})

	case PermissionAutoDeny:
		outcome := PermissionOutcome{Outcome: "cancelled"}
		if cls.OptionID != "" {
			outcome = PermissionOutcome{Outcome: "selected", OptionID: cls.OptionID}
		}
		_ = s.conn.writeResponse(rpcID, RequestPermissionResult{Outcome: outcome})

	case PermissionAskUserInput:
		s.mu.Lock()
		s.pendingPerms[requestID] = pendingPermission{
			rpcID:       append(json.RawMessage(nil), rpcID...),
			optionByKey: cls.OptionByKey,
			isUserInput: true,
		}
		s.mu.Unlock()
		s.approvals.Track(requestID, provider.EventUserInputResolved, cls.UserInput.Questions)
		meta, _ := json.Marshal(cls.UserInput)
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventUserInputRequest,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			ItemID:    requestID,
			Meta:      meta,
			Timestamp: time.Now(),
		})

	case PermissionAskUser:
		s.mu.Lock()
		s.pendingPerms[requestID] = pendingPermission{
			rpcID:       append(json.RawMessage(nil), rpcID...),
			optionByKey: cls.OptionByKey,
		}
		s.mu.Unlock()
		s.approvals.Track(requestID, provider.EventApprovalResolved, nil)
		meta, _ := json.Marshal(cls.Approval)
		s.emitEvent(provider.ProviderEvent{
			Kind:      provider.EventApprovalRequest,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			ItemID:    requestID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}
}

func (s *Session) emitEvent(evt provider.ProviderEvent) {
	if s.onEvent != nil {
		s.onEvent(evt)
	}
}

func buildPromptBlocks(content string, attachments []provider.ImageAttachment) []ContentBlock {
	parts := provider.SplitContentByImageMarkers(content, len(attachments))
	blocks := make([]ContentBlock, 0, len(parts))
	for _, part := range parts {
		if part.ImageIndex < 0 {
			if part.Text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: part.Text})
			}
			continue
		}
		if part.ImageIndex >= len(attachments) {
			continue
		}
		att := attachments[part.ImageIndex]
		if len(att.Data) == 0 {
			continue
		}
		mime := strings.TrimSpace(att.MimeType)
		if mime == "" {
			mime = "image/png"
		}
		blocks = append(blocks, ContentBlock{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(att.Data),
			MimeType: mime,
		})
	}
	return blocks
}

func permissionRequestID(rpcID json.RawMessage, toolCallID string) string {
	raw := strings.TrimSpace(string(rpcID))
	raw = strings.Trim(raw, `"`)
	if raw != "" {
		return "acp-perm-" + raw
	}
	if toolCallID != "" {
		return "acp-perm-" + toolCallID
	}
	return fmt.Sprintf("acp-perm-%d", time.Now().UnixNano())
}

func currentConfigValue(opts []ConfigOption, id string) string {
	for _, opt := range opts {
		if opt.ID == id {
			return opt.CurrentValue
		}
	}
	return ""
}

func optionOffered(opts []ConfigOption, configID, value string) bool {
	for _, opt := range opts {
		if opt.ID != configID {
			continue
		}
		for _, choice := range opt.Options {
			if choice.Value == value {
				return true
			}
		}
	}
	return false
}

func resolveUserInputOptionID(answers map[string]provider.UserInputAnswer, byKey map[string]string) string {
	for _, ans := range answers {
		for _, val := range ans {
			if optID, ok := byKey[val]; ok && optID != "" {
				return optID
			}
		}
	}
	return ""
}

func formatToolQuestionAnswer(questions []provider.UserInputQuestion, answers map[string]provider.UserInputAnswer, decision string) string {
	if decision == "decline" {
		return "Cancelled."
	}
	if len(questions) == 1 {
		q := questions[0]
		if ans := answers[q.ID]; len(ans) > 0 {
			// Match numbered option index if possible for concise ACP answers.
			if !q.MultiSelect && len(ans) == 1 {
				for idx, opt := range q.Options {
					if opt.Label == ans[0] {
						return strconv.Itoa(idx + 1)
					}
				}
			}
			return strings.Join(ans, ", ")
		}
	}
	var parts []string
	for _, q := range questions {
		if ans := answers[q.ID]; len(ans) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", q.Question, strings.Join(ans, ", ")))
		}
	}
	if len(parts) == 0 {
		for _, ans := range answers {
			if len(ans) > 0 {
				parts = append(parts, strings.Join(ans, ", "))
			}
		}
	}
	return strings.Join(parts, "\n")
}
