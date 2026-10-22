// Read-aloud: one reply at a time across the app, spoken by this device's
// voice (`native/speech.ts`).
//
// A reply is read as its sentences (`utils/speakableText.ts`). Pausing
// stops the voice and remembers the sentence it was in; resuming starts
// that sentence again. Reading another reply stops this one and forgets
// its place. Finishing a reply returns it to the start.

import { untrack } from 'svelte';
import { speechEngine, type SpeechEngine, type SpeechPlayback, type SpeechVoice } from '../native/speech';
import { speakablePieces } from '../utils/speakableText';
import { readAloudPreferences } from './readAloudPreferences.svelte';
import { addErrorToast } from './toast.svelte';

export type ReadAloudPhase = 'idle' | 'playing' | 'paused';

const player = $state<{ key: string | null; phase: ReadAloudPhase; index: number }>({ key: null, phase: 'idle', index: 0 });
const voices = $state<{ list: SpeechVoice[] }>({ list: [] });

let engine: SpeechEngine | null = null;
let pieces: string[] = [];
let playback: SpeechPlayback | null = null;
let watchingVoices = false;

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

/** Whether replies offer read-aloud here: it is on and the device has a
 * voice. */
export function readAloudAvailable(): boolean {
  return readAloudPreferences().enabled && voices.list.length > 0;
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

/** Read `markdown` aloud as `key` from its first sentence, stopping
 * whatever is being read. */
export function readAloud(key: string, markdown: string): void {
  if (!engine || !readAloudAvailable()) return;
  stopVoice();
  pieces = speakablePieces(markdown);
  player.key = key;
  player.index = 0;
  if (pieces.length === 0) {
    player.phase = 'idle';
    player.key = null;
    return;
  }
  speakFrom(0);
}

/** Stop the voice and keep the place, at the start of its sentence. */
export function pauseReadAloud(): void {
  if (player.phase !== 'playing') return;
  stopVoice();
  player.phase = 'paused';
}

/** Read on from the start of the sentence it paused in. */
export function resumeReadAloud(): void {
  if (player.phase !== 'paused' || !readAloudAvailable()) return;
  speakFrom(player.index);
}

/** Read the paused reply again from its first sentence. */
export function restartReadAloud(): void {
  if (player.phase !== 'paused' || !readAloudAvailable()) return;
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
  if (!engine) return;
  const prefs = readAloudPreferences();
  player.index = index;
  player.phase = 'playing';
  // An engine reports the first piece before `speak` returns.
  let current: SpeechPlayback | null = null;
  current = engine.speak(pieces, index, { voiceId: prefs.voiceId, rate: prefs.rate, volume: prefs.volume }, (at) => {
    if (current === null || playback === current) player.index = at;
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
      addErrorToast('Read aloud stopped: this device’s voice failed.', err);
    },
  );
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
  pieces = [];
}

// Turning read-aloud off, or the device losing its voices, stops a reading
// in progress and forgets its place.
$effect.root(() => {
  $effect(() => {
    if (!readAloudAvailable()) untrack(stopReadAloud);
  });
});

/** Forget everything, including the engine. Tests only. */
export function resetReadAloudForTest(): void {
  stopReadAloud();
  engine = null;
  watchingVoices = false;
  voices.list = [];
}
