// Read-aloud: one reply at a time across the app, spoken by this device's
// voice (`native/speech.ts`) or by an OpenAI voice the reply's computer
// makes (`native/clipSpeech.ts`, `readAloudSource.svelte.ts`).
//
// A reply is read as its sentences (`utils/speakableText.ts`). Pausing
// stops the voice and remembers the sentence it was in; resuming starts
// that sentence again. Reading another reply stops this one and forgets
// its place. Finishing a reply returns it to the start.
//
// Each reading chooses its voice when it starts, and resume and restart
// keep that choice: a saved (or overriding) `openai:` voice when the
// reading's computer offers it, otherwise this device's voice, otherwise
// the computer's OpenAI voice on a device that has none.

import { untrack } from 'svelte';
import { speechEngine, type SpeechEngine, type SpeechPlayback, type SpeechVoice } from '../native/speech';
import { clipSpeechEngine } from '../native/clipSpeech';
import { speakablePieces } from '../utils/speakableText';
import type { BackendKey } from '../transport/backendKey';
import { readAloudPreferences } from './readAloudPreferences.svelte';
import { readAloudClipFetcher, readAloudSourceVoices, refreshReadAloudSource } from './readAloudSource.svelte';
import { addErrorToast } from './toast.svelte';

export type ReadAloudPhase = 'idle' | 'playing' | 'paused';

/** How one reading speaks: this device's voice, or an OpenAI voice of the
 * computer it belongs to. */
export type ReadAloudRoute =
  | { kind: 'device'; voiceId: string }
  | { kind: 'openai'; backend: BackendKey; voice: string };

/** The saved voice id prefix of an OpenAI voice. */
export const OPENAI_VOICE_PREFIX = 'openai:';

export function openAIVoiceId(voice: string): string {
  return `${OPENAI_VOICE_PREFIX}${voice}`;
}

/** The OpenAI voice a saved voice id names, or null for a device voice. */
export function openAIVoiceOf(voiceId: string): string | null {
  return voiceId.startsWith(OPENAI_VOICE_PREFIX) ? voiceId.slice(OPENAI_VOICE_PREFIX.length) : null;
}

const player = $state<{
  key: string | null;
  phase: ReadAloudPhase;
  index: number;
  // True from the start of a reading through OpenAI until its first
  // sentence sounds.
  waiting: boolean;
  route: ReadAloudRoute | null;
}>({ key: null, phase: 'idle', index: 0, waiting: false, route: null });
const voices = $state<{ list: SpeechVoice[] }>({ list: [] });

let engine: SpeechEngine | null = null;
let pieces: string[] = [];
let playback: SpeechPlayback | null = null;
// Counts readings started, so a late failure report knows it is stale.
let readings = 0;
let watchingVoices = false;

const clipEngineFor = (backend: BackendKey): SpeechEngine => clipSpeechEngine({
  voices: () => readAloudSourceVoices(backend).map((voice) => ({ id: voice.id, name: voice.name, lang: '', isDefault: false })),
  fetchClip: readAloudClipFetcher(backend),
});
let openAIEngineFor: (backend: BackendKey) => SpeechEngine = clipEngineFor;

/** Start listening for this device's voices. Idempotent; the app calls it
 * at boot. */
export function watchReadAloudVoices(source: SpeechEngine | null = speechEngine()): void {
  if (watchingVoices) return;
  watchingVoices = true;
  engine = source;
  if (!engine) return;
  const refresh = (): void => {
    void engine?.voices().then((list) => {
      voices.list = list;
    });
  };
  engine.onVoicesChanged(refresh);
  refresh();
}

/** This device's voices; empty until known, and on a device with none. */
export function readAloudVoices(): readonly SpeechVoice[] {
  return voices.list;
}

function offersOpenAIVoice(backend: BackendKey, voice: string): boolean {
  return readAloudSourceVoices(backend).some((v) => v.id === voice);
}

/** Whether replies of `backend` (null: no computer) offer read-aloud here:
 * it is on, and the device has a voice or that computer offers OpenAI
 * voices. */
export function readAloudAvailable(backend: BackendKey | null): boolean {
  if (!readAloudPreferences().enabled) return false;
  return voices.list.length > 0 || (backend !== null && readAloudSourceVoices(backend).length > 0);
}

/** How a reading of `backend`'s text in `voiceId` would speak now, or null
 * when nothing can read it. */
export function readAloudRoute(backend: BackendKey | null, voiceId: string): ReadAloudRoute | null {
  if (!readAloudPreferences().enabled) return null;
  const wanted = openAIVoiceOf(voiceId);
  if (backend !== null && wanted !== null && offersOpenAIVoice(backend, wanted)) {
    return { kind: 'openai', backend, voice: wanted };
  }
  if (engine && voices.list.length > 0) return { kind: 'device', voiceId: wanted === null ? voiceId : '' };
  const offered = backend === null ? [] : readAloudSourceVoices(backend);
  if (backend !== null && offered.length > 0) return { kind: 'openai', backend, voice: offered[0].id };
  return null;
}

function servable(route: ReadAloudRoute): boolean {
  if (!readAloudPreferences().enabled) return false;
  return route.kind === 'device' ? engine !== null && voices.list.length > 0 : offersOpenAIVoice(route.backend, route.voice);
}

/** The reading key of a reply. Item ids are thread-local, so the thread
 * is part of it. */
export function replyReadingKey(threadId: string, itemId: string): string {
  return `reply:${threadId}:${itemId}`;
}

/** `key`'s phase: idle unless it is the reply being read. */
export function readAloudPhase(key: string): ReadAloudPhase {
  return player.key === key ? player.phase : 'idle';
}

/** Whether `key` is playing but its first sentence has not sounded yet. */
export function readAloudWaiting(key: string): boolean {
  return player.key === key && player.phase === 'playing' && player.waiting;
}

/** The key being read or paused, or null. */
export function readAloudKey(): string | null {
  return player.key;
}

/** Read `markdown` aloud as `key` from its first sentence, stopping
 * whatever is being read. `backend` is the computer the text belongs to;
 * `voiceId` overrides the saved voice for this reading only. */
export function readAloud(key: string, markdown: string, backend: BackendKey | null, voiceId?: string): void {
  const route = readAloudRoute(backend, voiceId ?? readAloudPreferences().voiceId);
  if (!route) return;
  stopVoice();
  readings += 1;
  pieces = speakablePieces(markdown);
  if (pieces.length === 0) {
    reset();
    return;
  }
  player.key = key;
  player.route = route;
  speakFrom(0);
}

/** Stop the voice and keep the place, at the start of its sentence. */
export function pauseReadAloud(): void {
  if (player.phase !== 'playing') return;
  stopVoice();
  player.phase = 'paused';
  player.waiting = false;
}

/** Read on from the start of the sentence it paused in. */
export function resumeReadAloud(): void {
  if (player.phase !== 'paused' || !player.route || !servable(player.route)) return;
  speakFrom(player.index);
}

/** Read the paused reply again from its first sentence. */
export function restartReadAloud(): void {
  if (player.phase !== 'paused' || !player.route || !servable(player.route)) return;
  speakFrom(0);
}

/** Stop and forget the place. */
export function stopReadAloud(): void {
  stopVoice();
  reset();
}

/** The chord: pause what is playing or resume what is paused. False when
 * nothing is being read. */
export function toggleReadAloud(): boolean {
  if (player.phase === 'playing') pauseReadAloud();
  else if (player.phase === 'paused') resumeReadAloud();
  else return false;
  return true;
}

function speakFrom(index: number): void {
  const route = player.route;
  const speaker = route?.kind === 'openai' ? openAIEngineFor(route.backend) : engine;
  if (!route || !speaker) return;
  const prefs = readAloudPreferences();
  player.index = index;
  player.phase = 'playing';
  player.waiting = route.kind === 'openai';
  const voiceId = route.kind === 'openai' ? route.voice : route.voiceId;
  // An engine may report the first piece before `speak` returns.
  let current: SpeechPlayback | null = null;
  current = speaker.speak(pieces, index, { voiceId, rate: prefs.rate, volume: prefs.volume }, (at) => {
    if (current !== null && playback !== current) return;
    player.index = at;
    player.waiting = false;
  });
  playback = current;
  current.done.then(
    () => {
      // Pausing and stopping drop the handle before they stop the voice,
      // so a current playback that ends, finished or stopped from outside
      // (another page cancelling speech), is over.
      if (playback !== current) return;
      playback = null;
      reset();
    },
    (err: unknown) => {
      if (playback !== current) return;
      playback = null;
      reset();
      if (route.kind === 'device') addErrorToast('Read aloud stopped: this device’s voice failed.', err);
      else void reportOpenAIFailure(route, err);
    },
  );
}

// A failed OpenAI reading asks its computer again first. A voice it no
// longer offers went away, which stops a reading silently; any other
// failure says the computer's reason, unless another reading has started.
async function reportOpenAIFailure(route: ReadAloudRoute & { kind: 'openai' }, err: unknown): Promise<void> {
  const reading = readings;
  await refreshReadAloudSource(route.backend);
  if (reading !== readings || !servable(route)) return;
  addErrorToast(`Read aloud stopped: ${err instanceof Error ? err.message : String(err)}`, err);
}

function stopVoice(): void {
  const current = playback;
  playback = null;
  current?.stop();
}

function reset(): void {
  player.key = null;
  player.phase = 'idle';
  player.index = 0;
  player.waiting = false;
  player.route = null;
  pieces = [];
}

// Turning read-aloud off, the device losing its voices, or the reading's
// computer no longer offering its OpenAI voice stops a reading in progress
// and forgets its place.
$effect.root(() => {
  $effect(() => {
    const route = player.route;
    if (route && !servable(route)) untrack(stopReadAloud);
  });
});

/** Forget everything, including the engine. Tests only. */
export function resetReadAloudForTest(): void {
  stopReadAloud();
  engine = null;
  watchingVoices = false;
  voices.list = [];
  readings = 0;
  openAIEngineFor = clipEngineFor;
}

/** Read OpenAI voices through `factory`'s engine. Tests only. */
export function useOpenAIEngineForTest(factory: (backend: BackendKey) => SpeechEngine): void {
  openAIEngineFor = factory;
}
