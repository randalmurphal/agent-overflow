// The other half of the SHARED decision table.
//
// `internal/notify/testdata/gate_cases.json` is run case for case by
// `internal/app/app_notification_gate_cases_test.go` against the Go gate and
// here against `./gate.ts`. Neither file owns it: it sits in `internal/notify`
// because that package owns the wire contract both gates read.
//
// STRICT ON THE WAY IN, the same as the Go runner. A case this side cannot
// decode FAILS rather than being skipped — a case added for a preference only
// one gate knows about would otherwise pass there and quietly not run here,
// which is the drift the shared file exists to prevent.
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

import { SETTINGS_DEFAULTS } from '../generated/settingsDefaults';
import { defaultSettings } from '../stores/settingsDefaults';
import {
  notificationRefusal,
  soundCueFor,
  type NotificationGateSend,
  type NotificationGateSettings,
  type NotificationRefusal,
  type NotificationScreenFacts,
} from './gate';

const GATE_CASES = resolve(
  dirname(fileURLToPath(import.meta.url)),
  '../../../../internal/notify/testdata/gate_cases.json',
);

/** The answers a case may name. Anything else is a case this side cannot run. */
const REFUSALS: readonly NotificationRefusal[] = ['suppressed', 'hidden_thread', 'screen_attended'];

/** Every key a case object may carry, and every key of its parts. */
const CASE_KEYS = ['name', 'settings', 'send', 'screen', 'refusal', 'cue'];
const SEND_KEYS = ['kind', 'hiddenThread', 'target'];
const TARGET_KEYS = ['kind', 'threadId', 'workItemId'];
const SCREEN_KEYS = ['focused', 'threadVisible'];

interface GateCase {
  name: string;
  settings: Record<string, unknown>;
  send: NotificationGateSend;
  screen: NotificationScreenFacts;
  refusal: NotificationRefusal | null;
  cue: string | null;
}

function requireObject(value: unknown, what: string): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${what} must be a JSON object, got ${JSON.stringify(value)}`);
  }
  return value as Record<string, unknown>;
}

function requireKeys(value: Record<string, unknown>, allowed: readonly string[], what: string): void {
  for (const key of Object.keys(value)) {
    if (!allowed.includes(key)) throw new Error(`${what} carries ${key}, which this gate has no answer for`);
  }
}

function requireBoolean(value: unknown, what: string): boolean {
  if (typeof value !== 'boolean') throw new Error(`${what} must be a boolean, got ${JSON.stringify(value)}`);
  return value;
}

function requireString(value: unknown, what: string): string {
  if (typeof value !== 'string') throw new Error(`${what} must be a string, got ${JSON.stringify(value)}`);
  return value;
}

function parseCase(raw: unknown, index: number): GateCase {
  const entry = requireObject(raw, `case ${index}`);
  requireKeys(entry, CASE_KEYS, `case ${index}`);
  const name = requireString(entry.name, `case ${index} name`);

  const patch = requireObject(entry.settings, `${name} settings`);
  for (const key of Object.keys(patch)) {
    // The generated mirror of Go's DefaultSettings is the key list: a patch
    // naming something outside it would silently test the defaults instead
    // of the preference it names.
    if (!Object.hasOwn(SETTINGS_DEFAULTS, key)) {
      throw new Error(`${name} patches ${key}, which is not a settings key this build has`);
    }
  }

  const send = requireObject(entry.send, `${name} send`);
  requireKeys(send, SEND_KEYS, `${name} send`);
  const target = requireObject(send.target, `${name} send target`);
  requireKeys(target, TARGET_KEYS, `${name} send target`);

  const screen = requireObject(entry.screen, `${name} screen`);
  requireKeys(screen, SCREEN_KEYS, `${name} screen`);

  if (entry.refusal !== null && !REFUSALS.includes(entry.refusal as NotificationRefusal)) {
    throw new Error(`${name} expects refusal ${JSON.stringify(entry.refusal)}, which this gate never answers`);
  }
  if (entry.cue !== null && requireString(entry.cue, `${name} cue`) === '') {
    throw new Error(`${name} expects an empty cue; use null for no cue`);
  }

  return {
    name,
    settings: patch,
    send: {
      kind: requireString(send.kind, `${name} send kind`),
      hiddenThread: send.hiddenThread === undefined
        ? false
        : requireBoolean(send.hiddenThread, `${name} send hiddenThread`),
      target: { kind: requireString(target.kind, `${name} send target kind`) },
    },
    screen: {
      focused: requireBoolean(screen.focused, `${name} screen focused`),
      threadVisible: requireBoolean(screen.threadVisible, `${name} screen threadVisible`),
    },
    refusal: entry.refusal as NotificationRefusal | null,
    cue: entry.cue as string | null,
  };
}

function loadGateCases(): GateCase[] {
  const parsed: unknown = JSON.parse(readFileSync(GATE_CASES, 'utf8'));
  if (!Array.isArray(parsed)) throw new Error('the shared gate table must be a JSON array of cases');
  if (parsed.length === 0) throw new Error('the shared gate table holds no cases, so neither gate is under test');
  return parsed.map(parseCase);
}

describe('the shared gate table', () => {
  const cases = loadGateCases();

  it.each(cases.map((entry) => [entry.name, entry] as const))('%s', (_name, entry) => {
    const settings = { ...defaultSettings(), ...entry.settings } as NotificationGateSettings;
    expect(notificationRefusal(settings, entry.send, entry.screen)).toBe(entry.refusal);
    expect(soundCueFor(settings, entry.send.kind)).toBe(entry.cue);
  });

  // The table is worth nothing if it can go empty or lose a column without
  // anybody noticing, so the runner asserts its own shape too.
  it('covers every kind, every quiet-when reading and both hidden-thread answers', () => {
    const kinds = new Set(cases.map((entry) => entry.send.kind));
    for (const kind of [
      'turn-complete', 'approval-needed', 'error',
      'provider-signed-out', 'workflow-attention', 'app-update',
    ]) expect(kinds, `no case sends ${kind}`).toContain(kind);

    const readings = new Set(cases.map((entry) => entry.settings.notifyQuietWhen ?? SETTINGS_DEFAULTS.notifyQuietWhen));
    for (const reading of ['never', 'focused', 'threadVisible', 'focusedAndThreadVisible']) {
      expect(readings, `no case reads quiet-when ${reading}`).toContain(reading);
    }

    const answers = new Set(cases.map((entry) => entry.refusal));
    for (const answer of [null, ...REFUSALS]) expect(answers).toContain(answer);

    expect(cases.some((entry) => entry.cue === 'system'), 'no case resolves the system cue').toBe(true);
    expect(cases.some((entry) => entry.cue?.startsWith('custom:')), 'no case resolves a custom cue').toBe(true);
    expect(cases.some((entry) => entry.cue === null), 'no case resolves to silence').toBe(true);
    expect(cases.some((entry) => entry.send.hiddenThread === true), 'no case sends a hidden thread').toBe(true);
  });
});
