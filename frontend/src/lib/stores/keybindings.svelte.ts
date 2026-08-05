// Keybindings store: loads bindings from the backend, parses them, dispatches
// matching chords to the command registry via runCommand().
//
// Design notes
//   - Parsing happens once per (re)load. Parse errors land in `issues` and are
//     surfaced in the settings tab; the broken entry is skipped at runtime so
//     the rest of the config keeps working.
//   - Dispatch is a reverse-iteration first-match over the resolved list:
//     user overrides take precedence because the Go merge appends them last.
//   - The store deliberately does *not* own the DOM listener. App.svelte
//     installs a single window-level keydown that calls dispatchKey(ev, ctx).
//     That keeps this store testable without mounting any component.

import { addToast } from './toast.svelte';
import { GetKeybindings, Keybinding, ResetKeybindings, UpdateKeybindings } from './bindings';
import { isCommandEnabled, runCommand, type CommandContext } from './commandRegistry.svelte';
import type { Chord, WhenNode } from './keybindingParser';
import {
  chordMatches,
  encodeChord,
  macOptionLetterFromCode,
  macShiftedGlyphFromCode,
  tryParseChord,
  tryParseWhen,
} from './keybindingParser';
import { isMacPlatform } from '../utils/platform';

export interface KeybindingRule {
  key: string;
  command: string;
  when?: string;
  defaultId?: string;
  defaultKey?: string;
}

export interface ResolvedKeybinding {
  rule: KeybindingRule;
  chord: Chord;
  whenAst: WhenNode | null;
}

export interface KeybindingIssue {
  index: number;
  rule: KeybindingRule;
  reason: string;
}

/**
 * The `key` value meaning "this command is deliberately bound to nothing".
 * Mirrors `keybindings.Unbound` in Go (`internal/keybindings/AGENTS.md`
 * documents the encoding): "no chord" is a value of `key` rather than a
 * second field that could disagree with it.
 *
 * A rule carrying this is NOT the same as no rule at all. No rule means
 * "use the shipped default"; this means "suppress the shipped default",
 * so nothing compiles into `resolved` and nothing dispatches.
 */
export const UNBOUND_CHORD = '';

/**
 * True when `key` clears its command's chord instead of naming one. Trims,
 * so a hand-edited whitespace key reads the same as the canonical form the
 * backend persists.
 */
export function isUnboundChord(key: string | null | undefined): boolean {
  return (key ?? '').trim().length === 0;
}

let rules: KeybindingRule[] = $state([]);
let resolved: ResolvedKeybinding[] = $state([]);
let issues: KeybindingIssue[] = $state([]);
let loaded = $state(false);
const runtimeDefaults = new Map<string, KeybindingRule[]>();

function compileEffectiveRules(): void {
  const overriddenDefaults = new Set(rules.flatMap((rule) => rule.defaultId ? [rule.defaultId] : []));
  const defaults = Array.from(runtimeDefaults.values()).flat()
    .filter((rule) => !rule.defaultId || !overriddenDefaults.has(rule.defaultId));
  const compiled = compileAll([...defaults, ...rules]);
  resolved = compiled.resolved;
  issues = compiled.issues;
}

export function registerRuntimeKeybindingDefaults(owner: string, defaults: readonly KeybindingRule[]): () => void {
  runtimeDefaults.set(owner, defaults.slice());
  compileEffectiveRules();
  return () => {
    runtimeDefaults.delete(owner);
    compileEffectiveRules();
  };
}

/**
 * The identity of the shipped default row a rule occupies — the same identity
 * the Go merge resolves on (`internal/keybindings.Merge`: DefaultID first,
 * then the legacy command/context/original-key tuple). One user override may
 * exist per identity, so this is what a rebind replaces and what tells two
 * default rows of the SAME command apart (e.g. `thread.new.primary` vs
 * `thread.new.alternate`).
 */
export function keybindingIdentity(rule: KeybindingRule): string {
  const identityKey = rule.defaultId ?? rule.defaultKey ?? rule.key;
  return [rule.command, rule.when ?? '', identityKey].join('\0');
}

/** Canonical form of a chord string, so `Mod+Shift+X` and `shift+mod+x` compare equal. */
function canonicalChord(key: string): string {
  const chord = tryParseChord(key);
  return chord ? encodeChord(chord) : key.trim().toLowerCase();
}

/**
 * Find a row that would end up displaying `chord` for the same command AND
 * the same `when` context as `rule`, but on a different default row.
 *
 * Two such rows are indistinguishable in the settings table and only the
 * first is ever reachable (dispatch stops at the first match), so producing
 * one is never what the user asked for. `rule`'s own row is excluded by
 * identity, which keeps re-capturing a row's existing chord a no-op rather
 * than a self-collision. Overlap ACROSS commands is deliberately not
 * reported: that is a real (if sharp) user choice, and `when` contexts make
 * it legitimate.
 */
export function findDuplicateChordRow(
  rows: readonly KeybindingRule[],
  rule: KeybindingRule,
  chord: string,
): KeybindingRule | null {
  // Clearing a row can never collide: an unbound row is unreachable, so two
  // of them are not indistinguishable-in-the-table the way two rows sharing
  // a real chord are. Without this guard the canonical form of '' would
  // match every other unbound sibling and refuse the clear.
  if (isUnboundChord(chord)) return null;
  const identity = keybindingIdentity(rule);
  const when = (rule.when ?? '').trim();
  const wanted = canonicalChord(chord);
  for (const row of rows) {
    if (isUnboundChord(row.key)) continue;
    if (row.command !== rule.command) continue;
    if ((row.when ?? '').trim() !== when) continue;
    if (keybindingIdentity(row) === identity) continue;
    if (canonicalChord(row.key) === wanted) return row;
  }
  return null;
}

/**
 * The full effective rule list — every row the user can edit, in merge
 * order. Unlike `getResolvedKeybindings()` this keeps rows that compile to
 * nothing: an explicitly unbound row (so settings can show it and offer its
 * default back) and a row whose chord failed to parse (so it is fixable
 * rather than only reported in the issues callout). Settings renders this;
 * dispatch reads `resolved`.
 */
export function getKeybindingRules(): KeybindingRule[] {
  const overriddenDefaults = new Set(rules.flatMap((rule) => rule.defaultId ? [rule.defaultId] : []));
  return [
    ...Array.from(runtimeDefaults.values()).flat()
      .filter((rule) => !rule.defaultId || !overriddenDefaults.has(rule.defaultId)),
    ...rules,
  ];
}

/**
 * The compiled, dispatchable list — the pair of `getKeybindingRules()`. Rows
 * that are unbound or whose chord failed to parse are absent by construction,
 * which is what makes "unbound never fires" a property of the data rather
 * than a check every consumer has to remember.
 */
export function getResolvedKeybindings(): ResolvedKeybinding[] {
  return resolved;
}

export function getKeybindingIssues(): KeybindingIssue[] {
  return issues;
}

export function isKeybindingsLoaded(): boolean {
  return loaded;
}

/**
 * Return the chord string bound to a command id, or null when the command
 * has no chord — either nothing ever bound it, or the user explicitly
 * cleared it. Prefers the last-registered binding (user override) when
 * multiple entries match.
 *
 * This reads `resolved`, which unbound rules never enter, so a cleared
 * default cannot leak back out here. Callers MUST render nothing for null
 * rather than substituting a default chord: substituting is exactly the
 * bug the unbound state exists to fix.
 */
export function keybindingForCommand(commandId: string): string | null {
  for (let i = resolved.length - 1; i >= 0; i -= 1) {
    if (resolved[i].rule.command === commandId) {
      return resolved[i].rule.key;
    }
  }
  return null;
}

/**
 * Display-formatted chord for a command, or null when it has none.
 *
 * The one lookup every chord-hint surface should use: it makes the unbound
 * case unrepresentable-by-accident, where `formatChord(keybindingForCommand(id) ?? 'mod+b')`
 * both hard-codes a copy of the Go default and resurrects a chord the user
 * deliberately cleared.
 */
export function chordHintForCommand(commandId: string, isMac?: boolean): string | null {
  const key = keybindingForCommand(commandId);
  return key === null ? null : formatChord(key, isMac);
}

/**
 * Parenthesised chord suffix for a tooltip or aria-label — ` (⌘B)` — or the
 * empty string when the command has no chord, so the label reads cleanly
 * without a dangling "()".
 */
export function chordHintSuffix(commandId: string, isMac?: boolean): string {
  const hint = chordHintForCommand(commandId, isMac);
  return hint === null ? '' : ` (${hint})`;
}

/**
 * Return `rows` with the chord of the row matching `rule`'s default identity
 * replaced by `key` — the single list transform behind every settings edit:
 * rebind (a chord), clear (`UNBOUND_CHORD`), and restore-default
 * (`rule.defaultKey`). Rows are matched by identity, not index, so the
 * result is stable regardless of how the table is ordered.
 */
export function withReboundRow(
  rows: readonly KeybindingRule[],
  rule: KeybindingRule,
  key: string,
): KeybindingRule[] {
  const identity = keybindingIdentity(rule);
  const next: KeybindingRule[] = [];
  let replaced = false;
  for (const row of rows) {
    if (keybindingIdentity(row) === identity) {
      next.push({ ...row, key });
      replaced = true;
      continue;
    }
    next.push(row);
  }
  if (!replaced) {
    next.push({
      key,
      command: rule.command,
      when: rule.when,
      defaultId: rule.defaultId,
      defaultKey: rule.defaultKey,
    });
  }
  return next;
}

function compileAll(input: KeybindingRule[]): {
  resolved: ResolvedKeybinding[];
  issues: KeybindingIssue[];
} {
  const nextResolved: ResolvedKeybinding[] = [];
  const nextIssues: KeybindingIssue[] = [];
  input.forEach((rule, index) => {
    // Explicitly unbound: no chord to compile, and NOT a configuration
    // issue — the user asked for this. It stays out of `resolved`, which
    // is what keeps dispatch and every chord-hint lookup from falling back
    // to the default it replaced.
    if (isUnboundChord(rule.key)) return;
    const chord = tryParseChord(rule.key);
    if (!chord) {
      nextIssues.push({ index, rule, reason: `invalid shortcut "${rule.key}"` });
      return;
    }
    let whenAst: WhenNode | null = null;
    if (rule.when && rule.when.trim().length > 0) {
      whenAst = tryParseWhen(rule.when);
      if (!whenAst) {
        nextIssues.push({ index, rule, reason: `invalid when expression "${rule.when}"` });
        return;
      }
    }
    nextResolved.push({ rule, chord, whenAst });
  });
  return { resolved: nextResolved, issues: nextIssues };
}

export async function loadKeybindings(): Promise<void> {
  try {
    const raw = (await GetKeybindings()) as KeybindingRule[] | null;
    rules = Array.isArray(raw) ? raw : [];
    compileEffectiveRules();
    loaded = true;
  } catch (err) {
    console.error('Failed to load keybindings:', err);
    addToast('error', 'Failed to load keybindings');
    rules = [];
    resolved = [];
    issues = [];
    loaded = true;
  }
}

/**
 * Rules that carry no override and so must not be written to the user file.
 *
 * Two shapes:
 *  - back on its shipped chord — absence IS "use the default", and writing
 *    it would pin today's default against a future change to it;
 *  - unbound with no default to clear — it would silence nothing, and
 *    `keybindings.Service.Update` rejects an identity-less empty key
 *    precisely so a dropped chord can't masquerade as a deliberate clear.
 */
function isNonOverrideRule(rule: KeybindingRule): boolean {
  if (rule.defaultKey && rule.key === rule.defaultKey) return true;
  return isUnboundChord(rule.key) && !rule.defaultId && !rule.defaultKey;
}

/** Replace the persisted keybinding overrides with the provided effective rules. */
export async function saveKeybindings(next: KeybindingRule[]): Promise<void> {
  // The Wails-generated Keybinding class treats optional `when` as
  // `string | undefined` (required-but-maybe-absent) rather than `?:`, so we
  // normalize through the constructor to satisfy the binding's parameter type.
  const payload = next.filter((rule) => !isNonOverrideRule(rule)).map(
    (rule) =>
      new Keybinding({
        key: rule.key,
        command: rule.command,
        when: rule.when,
        defaultId: rule.defaultId,
        defaultKey: rule.defaultKey,
      }),
  );
  await UpdateKeybindings(payload);
  await loadKeybindings();
}

export async function resetKeybindingsToDefaults(): Promise<void> {
  await ResetKeybindings();
  await loadKeybindings();
}

/** Test / SSR helper — seeds the store without a backend round-trip. */
export function setKeybindingsForTest(input: KeybindingRule[]): void {
  rules = input.slice();
  compileEffectiveRules();
  loaded = true;
}

export function resetKeybindingsStore(): void {
  rules = [];
  resolved = [];
  issues = [];
  loaded = false;
  runtimeDefaults.clear();
}

/**
 * Marker attribute a chord-recording control stamps on itself
 * (KeybindingsSettings' capture button). While the keydown originates
 * inside one, the keystroke is DATA — the chord being recorded — and
 * must not also run whatever command it is currently bound to.
 *
 * The recorder additionally calls `stopPropagation`, which happens to
 * keep the event away from App.svelte's window listener today. That is
 * caller discipline: it lives in the recorder, not in dispatch, so the
 * next recorder that forgets it silently rebinds-and-fires. The guard
 * below is the structural half — recording `mod+b` records `mod+b`
 * instead of collapsing the sidebar no matter how the event travels.
 */
export const KEYBINDING_CAPTURE_ATTR = 'data-keybinding-capture';

const CAPTURE_SELECTOR = `[${KEYBINDING_CAPTURE_ATTR}]`;

export function isKeybindingCaptureTarget(target: EventTarget | null): boolean {
  const element = target as Element | null;
  if (!element || typeof element.closest !== 'function') return false;
  return element.closest(CAPTURE_SELECTOR) !== null;
}

/**
 * Try to dispatch a keyboard event to a bound command. Returns true if the
 * event was handled (caller should preventDefault/stopPropagation), false
 * otherwise.
 */
export function dispatchKey(
  event: KeyboardEvent,
  ctx: CommandContext,
  options: { isMac?: boolean } = {},
): boolean {
  if (isKeybindingCaptureTarget(event.target)) return false;
  const isMac = options.isMac ?? isMacPlatform();

  // Walk in reverse so user overrides — appended last by the Go merge — beat
  // matching default rules earlier in the list. First rule whose chord AND
  // `when` expression both match wins; command-level `when` (in the registry)
  // acts as a secondary gate.
  for (let i = resolved.length - 1; i >= 0; i -= 1) {
    const r = resolved[i];
    if (!chordMatches(r.chord, event, isMac)) continue;
    if (r.whenAst && !evaluateRuleWhen(r.whenAst, ctx)) continue;
    const handled = runCommand(r.rule.command, ctx);
    if (handled) return true;
    // Chord matched but the command was disabled; keep searching so a later
    // rule with the same chord and a different when can still fire.
  }
  return false;
}

export function eventMatchesKeybindingCommand(
  event: KeyboardEvent,
  ctx: CommandContext,
  commandIds: ReadonlySet<string>,
  options: { isMac?: boolean } = {},
): boolean {
  // Same answer as dispatchKey by construction: this predicate exists to
  // tell a caller "a command owns this keystroke", and inside a recorder
  // none does. Diverging would let the editable-target path in
  // App.svelte claim (and preventDefault) a chord dispatch then refuses.
  if (isKeybindingCaptureTarget(event.target)) return false;
  const isMac = options.isMac ?? isMacPlatform();

  for (let i = resolved.length - 1; i >= 0; i -= 1) {
    const r = resolved[i];
    if (!commandIds.has(r.rule.command)) continue;
    if (!chordMatches(r.chord, event, isMac)) continue;
    if (r.whenAst && !evaluateRuleWhen(r.whenAst, ctx)) continue;
    if (!isCommandEnabled(r.rule.command, ctx)) continue;
    return true;
  }
  return false;
}

// A context that models exactly one fact: a terminal is focused. The xterm key
// handler uses it to decide whether a chord should *escape* the terminal to the
// app. `when` evaluation only reads `flags`, and the pane-nav commands carry no
// command-level `when`, so modelling terminalFocus alone is sufficient — a
// chord still rule-gated on `!terminalFocus` evaluates false and is left for the
// shell, while an un-gated chord (the default alt+h/l pane-nav bindings) matches
// and escapes.
const TERMINAL_FOCUSED_CTX = { flags: { terminalFocus: true } } as unknown as CommandContext;

/**
 * Should this terminal keydown bubble to the app instead of the PTY? True when
 * the event matches one of `commandIds` whose binding stays enabled while a
 * terminal is focused. Reads the live resolved bindings, so user rebinds take
 * effect without remounting the terminal.
 */
export function eventEscapesTerminalToCommand(
  event: KeyboardEvent,
  commandIds: ReadonlySet<string>,
  options: { isMac?: boolean } = {},
): boolean {
  return eventMatchesKeybindingCommand(event, TERMINAL_FOCUSED_CTX, commandIds, options);
}

function evaluateRuleWhen(node: WhenNode, ctx: CommandContext): boolean {
  const flags = ctx.flags ?? ctx;
  switch (node.type) {
    case 'identifier':
      return flags[node.name] === true;
    case 'not':
      return !evaluateRuleWhen(node.node, ctx);
    case 'and':
      return evaluateRuleWhen(node.left, ctx) && evaluateRuleWhen(node.right, ctx);
    case 'or':
      return evaluateRuleWhen(node.left, ctx) || evaluateRuleWhen(node.right, ctx);
  }
}

/**
 * Format a chord for human display (platform-aware symbols on macOS).
 */
export function formatChord(key: string, isMac?: boolean): string {
  const mac = isMac ?? isMacPlatform();
  const chord = tryParseChord(key);
  if (!chord) return key;
  const parts: string[] = [];
  const modIsMeta = chord.modKey && mac;
  const modIsCtrl = chord.modKey && !mac;
  if (chord.ctrlKey || modIsCtrl) parts.push(mac ? '⌃' : 'Ctrl');
  if (chord.altKey) parts.push(mac ? '⌥' : 'Alt');
  if (chord.shiftKey) parts.push(mac ? '⇧' : 'Shift');
  if (chord.metaKey || modIsMeta) parts.push(mac ? '⌘' : 'Win');
  parts.push(chord.key === ' ' ? 'Space' : chord.key.length === 1 ? chord.key.toUpperCase() : chord.key);
  return mac ? parts.join('') : parts.join('+');
}

export function encodeChordFromEvent(event: KeyboardEvent, isMac?: boolean): string | null {
  const mac = isMac ?? isMacPlatform();
  // Ignore pure modifier keys — a chord needs an actual key.
  const key = eventKeyForChordCapture(event, mac);
  if (
    key === 'control' ||
    key === 'meta' ||
    key === 'shift' ||
    key === 'alt' ||
    key === 'os' ||
    key === 'super'
  ) {
    return null;
  }
  const chord: Chord = {
    key: key === ' ' ? 'space' : key,
    metaKey: event.metaKey && !mac ? false : event.metaKey,
    ctrlKey: event.ctrlKey,
    shiftKey: event.shiftKey,
    altKey: event.altKey,
    modKey: false,
  };
  // When both modKey-eligible flags are the platform default, normalize to mod.
  if (mac && chord.metaKey && !chord.ctrlKey) {
    chord.metaKey = false;
    chord.modKey = true;
  } else if (!mac && chord.ctrlKey && !chord.metaKey) {
    chord.ctrlKey = false;
    chord.modKey = true;
  }
  return encodeChord(chord);
}

function eventKeyForChordCapture(event: KeyboardEvent, isMac: boolean): string {
  if (isMac && event.altKey) {
    const optionLetter = macOptionLetterFromCode(event.code);
    if (optionLetter) return optionLetter;
  }
  // Mirror of the matcher's Cmd+Shift stripping fallback: recording
  // Cmd+Shift+` in a WKWebView delivers key "`", which would capture a
  // non-portable "cmd+shift+`" chord. Store the shifted glyph instead so
  // the captured binding matches what Chromium platforms report natively.
  if (isMac && event.metaKey && event.shiftKey) {
    const shifted = macShiftedGlyphFromCode(event.code);
    if (shifted && event.key.toLowerCase() !== shifted) return shifted;
  }
  return event.key.toLowerCase();
}
