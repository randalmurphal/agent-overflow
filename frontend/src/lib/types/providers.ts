// 'claude-tui' is the interactive Claude Code TUI driven in a PTY — a third,
// additive provider alongside headless 'claude' and 'codex'. It reuses claude's
// binary, auth, and model catalog; it differs only in that AO drives the real
// terminal (with take-control) rather than the headless stream. It is a
// first-class ProviderID (threads persist it, the model picker offers it when
// enabled), but it is intentionally absent from PROVIDER_SETTINGS_ORDER — it
// has no binary/path/account settings of its own. Its one setting is the
// `claudeTuiEnabled` flag, which defaults OFF and renders inside Claude's
// settings section; see `providerIsEnabled` in providers/catalog.ts.
export const PROVIDER_IDS = ['claude', 'codex', 'claude-tui'] as const;

export type ProviderID = (typeof PROVIDER_IDS)[number];

export function isProviderID(value: unknown): value is ProviderID {
  return value === 'claude' || value === 'codex' || value === 'claude-tui';
}

export function asProviderID(value: unknown): ProviderID | null {
  return isProviderID(value) ? value : null;
}
