import type { ReasoningEffort, Settings } from '../types/settings';
import {
  asProviderID,
  PROVIDER_IDS,
  type ProviderID,
} from '../types/providers';
export {
  asProviderID,
  isProviderID,
  PROVIDER_IDS,
  type ProviderID,
} from '../types/providers';

export type ProviderBackgroundStop =
  | 'claude-task'
  | 'codex-background-terminals'
  | 'none';

export interface ProviderEffortOption {
  value: ReasoningEffort;
  label: string;
}

// Per-provider affordance matrix. Each flag gates a user-facing capability the
// UI must hide or disable when the active thread's provider doesn't support it.
// claude-tui drives the real interactive TUI from outside the process, so the
// affordances AO mediates for the headless providers (runtime-mode selection,
// plan toggle, fork, composer attachments, MCP picker)
// have no meaning there — the human reaches them inside the terminal via
// take-control instead. Default (unknown provider) is "supported" so only an
// explicit `false` gates an affordance off; see `providerSupports`.
//
// Distinct from Go's `provider.Capabilities` (internal/provider/capabilities.go):
// that struct holds provider-process plumbing (model-catalog source,
// background-terminal cleaner); this holds UI-affordance gates. They are not
// mirrors of each other — keep them separate.
export interface ProviderCapabilities {
  // Runtime access modes (Supervised / Auto-accept edits / Full access).
  // false → the provider runs full-access only and the AccessToggle is omitted.
  runtimeModes: boolean;
  // chat ↔ plan interaction-mode toggle. false → chat only (no plan mode).
  planMode: boolean;
  // Fork-thread and fork-from-message affordances.
  fork: boolean;
  // Composer image/file attachments (button, paste, drag-drop).
  attachments: boolean;
  // MCP server selection + status picker.
  mcp: boolean;
}

const FULL_CAPABILITIES: ProviderCapabilities = {
  runtimeModes: true,
  planMode: true,
  fork: true,
  attachments: true,
  mcp: true,
};

// claude-tui drives the real TUI, so most AO-mediated affordances live inside
// the terminal (reached through take-control) and stay off here. Image
// attachments are the exception: AO pastes an attachment's file PATH into the
// TUI composer, where Claude's paste handler reads it into a real image block —
// so claude-tui DOES accept composer attachments.
const TUI_CAPABILITIES: ProviderCapabilities = {
  runtimeModes: false,
  planMode: false,
  fork: false,
  attachments: true,
  mcp: false,
};

export interface ProviderDefinition {
  id: ProviderID;
  label: string;
  cliLabel: string;
  shortLabel: string;
  badgeClass: string;
  installActionLabel: string;
  textGenerationDefaultModel: string;
  textGenerationEffortOptions: ProviderEffortOption[];
  contextLabels: {
    standard: string;
    extended: string;
  };
  // Another provider this one is spawned through — same binary, same auth,
  // same model catalog. A dependent provider is only available when its
  // parent is too (`providerIsEnabled`), and its enable toggle renders
  // inside the parent's settings page rather than on a page of its
  // own (`dependentProviders`). Absent for a provider that stands alone.
  dependsOnProvider?: ProviderID;
  // Row hint for that enable toggle. Only set where the generic
  // "allow new threads to use this provider" line would not explain what
  // turning the provider on actually adds; ProviderSettingsPage falls back
  // to the generic wording.
  enableHint?: string;
  settings: {
    // The provider's OWN enable flag — the key its Settings toggle writes.
    // Reads go through `providerIsEnabled`, never this key directly, so a
    // dependent provider's parent flag cannot be forgotten at a call site.
    enabledKey: 'claudeEnabled' | 'codexEnabled' | 'claudeTuiEnabled';
    pathKey: 'claudeBinaryPath' | 'codexBinaryPath';
    standardCompactKey:
      | 'claudeAutoCompactStandardPercent'
      | 'codexAutoCompactStandardPercent';
    extendedCompactKey:
      | 'claudeAutoCompactExtendedPercent'
      | 'codexAutoCompactExtendedPercent';
    customEnvKey: 'claudeCustomEnv' | 'codexCustomEnv';
  };
  backgroundStop: ProviderBackgroundStop;
  capabilities: ProviderCapabilities;
}

export const PROVIDER_DEFINITIONS: Record<ProviderID, ProviderDefinition> = {
  claude: {
    id: 'claude',
    label: 'Claude',
    cliLabel: 'Claude CLI',
    shortLabel: 'C',
    badgeClass: 'bg-accent/10 text-accent',
    installActionLabel: 'Install Claude CLI',
    textGenerationDefaultModel: 'claude-haiku-4-5',
    textGenerationEffortOptions: [
      { value: 'low', label: 'Low' },
      { value: 'medium', label: 'Medium' },
      { value: 'high', label: 'High' },
      { value: 'xhigh', label: 'xHigh' },
      { value: 'max', label: 'Max' },
    ],
    contextLabels: {
      standard: '200k',
      extended: '1m',
    },
    settings: {
      enabledKey: 'claudeEnabled',
      pathKey: 'claudeBinaryPath',
      standardCompactKey: 'claudeAutoCompactStandardPercent',
      extendedCompactKey: 'claudeAutoCompactExtendedPercent',
      customEnvKey: 'claudeCustomEnv',
    },
    backgroundStop: 'claude-task',
    capabilities: FULL_CAPABILITIES,
  },
  codex: {
    id: 'codex',
    label: 'Codex',
    cliLabel: 'Codex CLI',
    shortLabel: 'X',
    badgeClass: 'bg-provider-codex/10 text-provider-codex',
    installActionLabel: 'Install Codex CLI',
    textGenerationDefaultModel: 'gpt-5.6-luna',
    textGenerationEffortOptions: [
      { value: 'none', label: 'None' },
      { value: 'minimal', label: 'Minimal' },
      { value: 'low', label: 'Low' },
      { value: 'medium', label: 'Medium' },
      { value: 'high', label: 'High' },
      { value: 'xhigh', label: 'xHigh' },
      { value: 'max', label: 'Max' },
      { value: 'ultra', label: 'Ultra' },
    ],
    contextLabels: {
      standard: '272k',
      extended: '1m',
    },
    settings: {
      enabledKey: 'codexEnabled',
      pathKey: 'codexBinaryPath',
      standardCompactKey: 'codexAutoCompactStandardPercent',
      extendedCompactKey: 'codexAutoCompactExtendedPercent',
      customEnvKey: 'codexCustomEnv',
    },
    backgroundStop: 'codex-background-terminals',
    capabilities: FULL_CAPABILITIES,
  },
  // claude-tui reuses claude's binary, auth, model catalog, effort set, and
  // context tiers — the only difference is the interactive PTY surface. So its
  // settings keys point at claude's, and text-generation fields mirror claude's
  // (claude-tui is never selected for commit-message / title generation, which
  // is gated to claude + codex, but the type requires the fields). It has no
  // AO-managed background tasks — background work lives inside the TUI, reached
  // through take-control — so backgroundStop is 'none'.
  //
  // Its ONE key of its own is the enable flag, which defaults off (see
  // internal/settings.Settings.ClaudeTUIEnabled): the TUI surface is opt-in,
  // and `dependsOnProvider` ties it to claude so disabling Claude takes the
  // TUI with it.
  'claude-tui': {
    id: 'claude-tui',
    label: 'Claude TUI',
    cliLabel: 'Claude TUI',
    shortLabel: 'T',
    badgeClass: 'bg-provider-claude-tui/10 text-provider-claude-tui',
    installActionLabel: 'Install Claude CLI',
    dependsOnProvider: 'claude',
    enableHint:
      'Interactive terminal sessions driven through the real Claude TUI. Hidden from pickers when off.',
    textGenerationDefaultModel: 'claude-haiku-4-5',
    textGenerationEffortOptions: [
      { value: 'low', label: 'Low' },
      { value: 'medium', label: 'Medium' },
      { value: 'high', label: 'High' },
      { value: 'xhigh', label: 'xHigh' },
      { value: 'max', label: 'Max' },
    ],
    contextLabels: {
      standard: '200k',
      extended: '1m',
    },
    settings: {
      enabledKey: 'claudeTuiEnabled',
      pathKey: 'claudeBinaryPath',
      standardCompactKey: 'claudeAutoCompactStandardPercent',
      extendedCompactKey: 'claudeAutoCompactExtendedPercent',
      customEnvKey: 'claudeCustomEnv',
    },
    backgroundStop: 'none',
    capabilities: TUI_CAPABILITIES,
  },
};

// The providers that get a settings SECTION of their own — binary path, model
// catalog, accounts, context budgets. claude-tui is intentionally absent: it
// has none of those (it reuses claude's), and its one setting, the enable
// flag, renders inside claude's section via `dependentProviders`.
export const PROVIDER_SETTINGS_ORDER: ProviderID[] = ['claude', 'codex'];
// claude-tui follows claude in the model picker so the two Claude surfaces read
// as a pair. Membership here is layout only — whether an entry is actually
// OFFERED is `providerIsEnabled`.
export const PROVIDER_MODEL_MENU_ORDER: ProviderID[] = ['codex', 'claude', 'claude-tui'];

export function getProviderDefinition(provider: ProviderID): ProviderDefinition;
export function getProviderDefinition(
  provider: string | null | undefined,
): ProviderDefinition | null;
export function getProviderDefinition(
  provider: string | null | undefined,
): ProviderDefinition | null {
  const id = asProviderID(provider);
  return id ? PROVIDER_DEFINITIONS[id] : null;
}

export function providerLabel(provider: string | null | undefined): string {
  return getProviderDefinition(provider)?.label ?? provider ?? 'Provider';
}

export function providerCliLabel(provider: string | null | undefined): string {
  return getProviderDefinition(provider)?.cliLabel ?? providerLabel(provider);
}

// The enable flags a settings object must carry to answer `providerIsEnabled`.
// Derived from the definitions rather than spelled out, so adding a provider
// with a new flag can't leave a caller's type behind.
export type ProviderEnablementSettings = Pick<
  Settings,
  ProviderDefinition['settings']['enabledKey']
>;

// Whether a provider is currently offered — the ONE read of the enable flags.
// Every picker, menu and settings filter asks this instead of indexing
// `settings[definition.settings.enabledKey]`, because that read is wrong for a
// dependent provider: claude-tui runs claude's binary under claude's auth, so
// a disabled Claude has to take the TUI with it whatever the TUI's own flag
// says. Hand-ANDing that at each call site is exactly the rule a fourth caller
// forgets.
//
// Unknown / absent providers answer false, the opposite of `providerSupports`
// above, and deliberately: that function asks "does this EXISTING thread's
// provider withhold an affordance", where an unrecognised id must not subtract
// anything; this one asks "may we OFFER this provider", and there is nothing to
// offer for an id the catalog cannot describe.
//
// Availability is not permission to render history. A disabled provider's
// threads keep rendering, resuming and sending — nothing on the timeline or
// send path consults this; it gates the affordances that would start NEW work
// on the provider.
export function providerIsEnabled(
  settings: ProviderEnablementSettings,
  provider: string | null | undefined,
): boolean {
  let definition = getProviderDefinition(provider);
  // Walk the dependency chain iteratively, bounded by the catalog size: a
  // cycle introduced by a future edit answers "not offered" instead of
  // recursing until the stack gives out.
  for (let hops = 0; hops <= PROVIDER_IDS.length; hops += 1) {
    if (!definition) return false;
    if (!settings[definition.settings.enabledKey]) return false;
    const parent = definition.dependsOnProvider;
    if (parent === undefined) return true;
    definition = PROVIDER_DEFINITIONS[parent];
  }
  return false;
}

// Inverted once at module init: the catalog is static, and the settings
// template asks per provider on every recompute, so the answer has to be a
// lookup with a stable array identity, not a fresh filter+map each time.
const DEPENDENT_PROVIDERS = new Map<ProviderID, ProviderDefinition[]>();
const NO_DEPENDENTS: readonly ProviderDefinition[] = [];
for (const id of PROVIDER_IDS) {
  const parent = PROVIDER_DEFINITIONS[id].dependsOnProvider;
  if (parent === undefined) continue;
  const existing = DEPENDENT_PROVIDERS.get(parent);
  if (existing) existing.push(PROVIDER_DEFINITIONS[id]);
  else DEPENDENT_PROVIDERS.set(parent, [PROVIDER_DEFINITIONS[id]]);
}

// The providers whose enable toggle belongs on `parent`'s settings page —
// those spawned through its binary. Data-driven so a second dependent
// provider surfaces without ProviderSettingsPage learning any provider id.
export function dependentProviders(parent: ProviderID): readonly ProviderDefinition[] {
  return DEPENDENT_PROVIDERS.get(parent) ?? NO_DEPENDENTS;
}

// Whether a provider supports a given affordance. Unknown providers default to
// supported so the gate only ever subtracts a capability an explicit definition
// has turned off (today: claude-tui). Pass a thread/pane provider string.
export function providerSupports(
  provider: string | null | undefined,
  capability: keyof ProviderCapabilities,
): boolean {
  const definition = getProviderDefinition(provider);
  return definition ? definition.capabilities[capability] : true;
}
