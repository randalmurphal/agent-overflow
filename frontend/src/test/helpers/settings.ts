import type { Settings } from '../../lib/types/settings';

export function makeSettings(overrides: Partial<Settings> = {}): Settings {
  return {
    // No `theme` field: the light/dark mode moved out of settings into
    // `stores/appearance.svelte.ts` (docs/specs/theme-system.md §6.2).
    timestampFormat: 'locale',
    sansFont: 'geist',
    monoFont: 'geist',
    fontSize: 13,
    recentWorkspaces: [],
    diffWordWrap: false,
    collapseDiffPreviews: false,
    streamingEnabled: true,
    lowPowerMode: false,
    confirmArchive: true,
    confirmDelete: true,
    claudeBinaryPath: 'claude',
    codexBinaryPath: 'codex',
    claudeEnabled: true,
    codexEnabled: true,
    // Matches the shipped default: claude-tui is opt-in, so a test that wants
    // the TUI offered has to say so.
    claudeTuiEnabled: false,
    defaultThreadEnvMode: 'local',
    worktreeBranchPrefix: 'ao-',
    paneDensity: 'compact',
    activityRunDefault: 'expanded',
    activityRunWindowRows: 30,
    textGenerationProvider: 'codex',
    textGenerationModel: '',
    textGenerationReasoningEffort: 'low',
    commitMessageStyle: 'conventional',
    commitMessageStyleCustom: '',
    claudeAutoCompactStandardPercent: 90,
    claudeAutoCompactExtendedPercent: 90,
    codexAutoCompactStandardPercent: 90,
    codexAutoCompactExtendedPercent: 90,
    observabilityTracingEnabled: false,
    observabilityOtlpEndpoint: '',
    observabilityEventLogEnabled: false,
    network: { bindAll: false },
    retention: { days: 30 },
    backgroundGitFetch: true,
    gitlabSelfHostedHosts: [],
    projectSortMode: 'lastActivity',
    usagePeriod: 'month',
    workflowPaused: false,
    ...overrides,
  };
}
