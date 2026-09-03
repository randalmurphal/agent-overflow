// Generated from internal/settings.DefaultSettings by internal/settings/gendefaults.
// Do not edit; regenerate with `go generate ./internal/settings`.
//
// Zero values are materialized explicitly: Go's `omitempty` drops them on
// the wire, and mergeSettingsWithDefaults fills them back in from here.
// Fields the TS Settings type leaves optional and the frontend deliberately
// leaves undefined are on the generator's deny-list, not missing by accident.

import type { Settings } from '../types/settings';

export const SETTINGS_DEFAULTS = {
  timestampFormat: "locale",
  sansFont: "geist",
  monoFont: "geist",
  fontSize: 13,
  recentWorkspaces: [],
  diffWordWrap: true,
  collapseDiffPreviews: true,
  streamingEnabled: true,
  lowPowerMode: false,
  browserEnabled: true,
  browserPersistSiteData: true,
  browserAllowOutsideWorkspace: false,
  confirmArchive: true,
  confirmDelete: true,
  autoPinNewThreads: true,
  claudeBinaryPath: "claude",
  codexBinaryPath: "codex",
  claudeEnabled: true,
  codexEnabled: true,
  claudeTuiEnabled: false,
  claudeHiddenModels: [],
  codexHiddenModels: [],
  claudeCustomEnv: [],
  codexCustomEnv: [],
  defaultThreadEnvMode: "local",
  worktreeBranchPrefix: "ao-",
  paneDensity: "compact",
  activityRunDefault: "collapsed",
  activityRunWindowRows: 30,
  textGenerationProvider: "codex",
  textGenerationModel: "",
  textGenerationReasoningEffort: "low",
  commitMessageStyle: "conventional",
  commitMessageStyleCustom: "",
  claudeAutoCompactStandardPercent: 90,
  claudeAutoCompactExtendedPercent: 90,
  codexAutoCompactStandardPercent: 90,
  codexAutoCompactExtendedPercent: 90,
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: "",
  observabilityEventLogEnabled: false,
  network: {
    bindAll: false,
  },
  retention: {
    days: 30,
  },
  backgroundGitFetch: true,
  gitlabSelfHostedHosts: [],
  projectSortMode: "lastActivity",
  usagePeriod: "month",
  spinnerVerbsEnabled: true,
  spinnerAnimationsEnabled: false,
  spinnerCustomVerbs: [],
  spinnerBuiltinVerbsDisabled: false,
  spinnerDisabledAnimations: [],
  spinnerCompactionAnimation: "",
  workflowPaused: false,
  keepAwakeEnabled: false,
  keepAwakeScreen: true,
} satisfies Settings;
