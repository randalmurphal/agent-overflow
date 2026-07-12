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
  tryParseChord,
  tryParseWhen,
} from './keybindingParser';

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

export function getKeybindingRules(): KeybindingRule[] {
  const overriddenDefaults = new Set(rules.flatMap((rule) => rule.defaultId ? [rule.defaultId] : []));
  return [
    ...Array.from(runtimeDefaults.values()).flat()
      .filter((rule) => !rule.defaultId || !overriddenDefaults.has(rule.defaultId)),
    ...rules,
  ];
}

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
 * Return the chord string bound to a command id, if any. Prefers the
 * last-registered binding (user override) when multiple entries match.
 */
export function keybindingForCommand(commandId: string): string | null {
  for (let i = resolved.length - 1; i >= 0; i -= 1) {
    if (resolved[i].rule.command === commandId) {
      return resolved[i].rule.key;
    }
  }
  return null;
}

function compileAll(input: KeybindingRule[]): {
  resolved: ResolvedKeybinding[];
  issues: KeybindingIssue[];
} {
  const nextResolved: ResolvedKeybinding[] = [];
  const nextIssues: KeybindingIssue[] = [];
  input.forEach((rule, index) => {
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

function isUnchangedDefaultRule(rule: KeybindingRule): boolean {
  return Boolean(rule.defaultKey) && rule.key === rule.defaultKey;
}

/** Replace the persisted keybinding overrides with the provided effective rules. */
export async function saveKeybindings(next: KeybindingRule[]): Promise<void> {
  // The Wails-generated Keybinding class treats optional `when` as
  // `string | undefined` (required-but-maybe-absent) rather than `?:`, so we
  // normalize through the constructor to satisfy the binding's parameter type.
  const payload = next.filter((rule) => !isUnchangedDefaultRule(rule)).map(
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
 * Try to dispatch a keyboard event to a bound command. Returns true if the
 * event was handled (caller should preventDefault/stopPropagation), false
 * otherwise.
 */
export function dispatchKey(
  event: KeyboardEvent,
  ctx: CommandContext,
  options: { isMac?: boolean } = {},
): boolean {
  const isMac =
    options.isMac ??
    (typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform));

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
  const isMac =
    options.isMac ??
    (typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform));

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
  const mac = isMac ?? (typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform));
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
  const mac = isMac ?? (typeof navigator !== 'undefined' && /Mac|iPod|iPhone|iPad/.test(navigator.platform));
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
  return event.key.toLowerCase();
}
