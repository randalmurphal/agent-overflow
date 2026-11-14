// Read-aloud: one reply at a time across the app, spoken by this device's
// voice (`native/speech.ts`) or by an OpenAI voice the reply's computer
// makes (`native/clipSpeech.ts`, `readAloudSource.svelte.ts`).
//
// A reply is read in sections (`utils/speakableText.ts`). An OpenAI voice
// reads one section per clip; a device voice reads each section's
// sentences, and a table maps its sentence back to the section. Pausing an
// OpenAI voice holds its clip, so resuming continues mid-sentence; a
// device voice can only stop, so pausing remembers the sentence it was in
// and resuming starts that sentence again. Previous, next and "read from
// here" move by section, through the playback when it can skip and by
// speaking again from the section's first piece when it cannot; back moves
// READ_ALOUD_BACK_SECONDS when the voice can seek, otherwise to the
// sentence's start. Reading another reply stops this one and forgets its
// place. Finishing a reply returns it to the start.
//
// Each reading chooses its voice when it starts, and resume and restart
// keep that choice: a saved (or overriding) `openai:` voice when the
// reading's computer offers it, otherwise this device's voice, otherwise
// the computer's OpenAI voice on a device that has none. A reply's
// computer is the one its thread verifiably runs on; a reading through
// OpenAI stops when that stops being the computer it reads through.

import { untrack } from 'svelte';
import { speechEngine, type SpeechEngine, type SpeechPlayback, type SpeechVoice } from '../native/speech';
import { clipSpeechEngine } from '../native/clipSpeech';
import { speakableSections, type SpeakableSection } from '../utils/speakableText';
import type { BackendKey } from '../transport/backendKey';
import { verifiedThreadMachine } from './attachedBackends.svelte';
import { readAloudPreferences } from './readAloudPreferences.svelte';
import { isReadAloudSwitchedOff, readAloudClipFetcher, readAloudSourceVoices, refreshReadAloudSource } from './readAloudSource.svelte';
import { addErrorToast } from './toast.svelte';

export type ReadAloudPhase = 'idle' | 'playing' | 'paused';

/** How one reading speaks: this device's voice, or an OpenAI voice of the
 * computer it belongs to. */
export type ReadAloudRoute =
  | { kind: 'device'; voiceId: string }
  | { kind: 'openai'; backend: BackendKey; voice: string };

/** What a reading's text belongs to: a reply's thread, read through the
 * computer it verifiably runs on, or a computer named outright (Settings >
 * Voice). */
export type ReadAloudOwner = { thread: string } | { computer: BackendKey };

function ownerMachine(owner: ReadAloudOwner): BackendKey | null {
  return 'thread' in owner ? verifiedThreadMachine(owner.thread) : owner.computer;
}

/** The saved voice id prefix of an OpenAI voice. */
export const OPENAI_VOICE_PREFIX = 'openai:';

export function openAIVoiceId(voice: string): string {
  return `${OPENAI_VOICE_PREFIX}${voice}`;
}

/** The OpenAI voice a saved voice id names, or null for a device voice. */
export function openAIVoiceOf(voiceId: string): string | null {
  return voiceId.startsWith(OPENAI_VOICE_PREFIX) ? voiceId.slice(OPENAI_VOICE_PREFIX.length) : null;
}

/** Seconds `readAloudBack` moves back when the voice can seek. */
export const READ_ALOUD_BACK_SECONDS = 10;

const player = $state<{
  key: string | null;
  phase: ReadAloudPhase;
  // The piece sounding or about to sound: a section for an OpenAI route, a
  // sentence for a device route.
  index: number;
  // True from a start or a jump through OpenAI until its piece sounds.
  waiting: boolean;
  route: ReadAloudRoute | null;
  owner: ReadAloudOwner | null;
}>({ key: null, phase: 'idle', index: 0, waiting: false, route: null, owner: null });
const voices = $state<{ list: SpeechVoice[] }>({ list: [] });

// The reading's pieces and how they map to its sections. Replaced whole
// with every reading, so readers of it track the change.
interface Layout {
  pieces: string[];
  /** The section of each piece. */
  sectionOf: number[];
  /** The first piece of each section. */
  firstPiece: number[];
  blocks: readonly { first: number; last: number }[];
}
const EMPTY_LAYOUT: Layout = { pieces: [], sectionOf: [], firstPiece: [], blocks: [] };
let layout = $state.raw<Layout>(EMPTY_LAYOUT);

let engine: SpeechEngine | null = null;
// The voice of the reading in progress, kept while paused when it holds
// its place (it offers pause and resume).
let playback: SpeechPlayback | null = null;
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

// Whether a reading can go on: a device route while the device has a
// voice, an OpenAI route while its computer offers the voice and the text
// still belongs to that computer.
function servable(route: ReadAloudRoute, owner: ReadAloudOwner): boolean {
  if (!readAloudPreferences().enabled) return false;
  if (route.kind === 'device') return engine !== null && voices.list.length > 0;
  return offersOpenAIVoice(route.backend, route.voice) && ownerMachine(owner) === route.backend;
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

/** Whether `key` is playing but the piece it started or jumped to has not
 * sounded yet. */
export function readAloudWaiting(key: string): boolean {
  return player.key === key && player.phase === 'playing' && player.waiting;
}

/** The key being read or paused, or null. */
export function readAloudKey(): string | null {
  return player.key;
}

/** Where `key`'s reading is: the section sounding or about to sound, and
 * how many there are. Null unless `key` is the reply being read or paused. */
export function readAloudPosition(key: string): { section: number; sections: number } | null {
  if (player.key !== key || player.phase === 'idle') return null;
  return { section: currentSection(), sections: layout.blocks.length };
}

/** The top-level blocks each section of `key`'s reading covers, in order,
 * as indexes into `renderedBlocks`; empty unless `key` is the reply being
 * read or paused. */
export function readAloudSectionBlocks(key: string): readonly { first: number; last: number }[] {
  return player.key === key && player.phase !== 'idle' ? layout.blocks : EMPTY_LAYOUT.blocks;
}

/** Read `markdown` aloud as `key` from its first section, stopping
 * whatever is being read. `owner` is what the text belongs to; `voiceId`
 * overrides the saved voice for this reading only. */
export function readAloud(key: string, markdown: string, owner: ReadAloudOwner, voiceId?: string): void {
  const route = readAloudRoute(ownerMachine(owner), voiceId ?? readAloudPreferences().voiceId);
  if (!route) return;
  stopVoice();
  const sections = speakableSections(markdown);
  if (sections.length === 0) {
    reset();
    return;
  }
  layout = layoutOf(sections, route);
  player.key = key;
  player.route = route;
  player.owner = owner;
  speakFrom(0);
}

function layoutOf(sections: SpeakableSection[], route: ReadAloudRoute): Layout {
  const blocks = sections.map((section) => ({ first: section.firstBlock, last: section.lastBlock }));
  if (route.kind === 'openai') {
    const identity = sections.map((_, i) => i);
    return { pieces: sections.map((section) => section.text), sectionOf: identity, firstPiece: identity, blocks };
  }
  const pieces: string[] = [];
  const sectionOf: number[] = [];
  const firstPiece: number[] = [];
  sections.forEach((section, at) => {
    firstPiece.push(pieces.length);
    for (const sentence of section.sentences) {
      pieces.push(sentence);
      sectionOf.push(at);
    }
  });
  return { pieces, sectionOf, firstPiece, blocks };
}

/** Pause the voice. One that can hold its place holds it mid-sentence;
 * otherwise it stops and the place is the start of its sentence. */
export function pauseReadAloud(): void {
  if (player.phase !== 'playing') return;
  if (playback?.pause && playback.resume) {
    playback.pause();
    player.phase = 'paused';
    return;
  }
  stopVoice();
  player.phase = 'paused';
  player.waiting = false;
}

/** Read on from where it paused: mid-sentence when the voice held its
 * place, otherwise from the start of the sentence. */
export function resumeReadAloud(): void {
  if (!resumable()) return;
  if (playback?.resume) {
    player.phase = 'playing';
    playback.resume();
    return;
  }
  speakFrom(player.index);
}

/** Read the paused reply again from its first section. */
export function restartReadAloud(): void {
  if (!resumable()) return;
  jumpTo(0);
}

/** Read on from the start of `section` (0-based) of the reply being read
 * or paused, and play. Does nothing when nothing is being read or
 * `section` is out of range. */
export function readAloudFromSection(section: number): void {
  if (!movable() || !Number.isInteger(section) || section < 0 || section >= layout.firstPiece.length) return;
  jumpTo(layout.firstPiece[section]);
}

/** The section before the current one; in the first section, its start. */
export function readAloudPreviousSection(): void {
  if (!movable()) return;
  readAloudFromSection(Math.max(0, currentSection() - 1));
}

/** The section after the current one; nothing in the last. */
export function readAloudNextSection(): void {
  if (!movable()) return;
  readAloudFromSection(currentSection() + 1);
}

/** What `readAloudBack` does for the reading in progress: 'seconds' when
 * the voice can move back READ_ALOUD_BACK_SECONDS within the section,
 * 'sentence' when it can only start the sentence again, null when nothing
 * is being read. */
export function readAloudBackStep(): 'seconds' | 'sentence' | null {
  if (player.phase === 'idle') return null;
  return playback?.seekBack ? 'seconds' : 'sentence';
}

/** Move back: READ_ALOUD_BACK_SECONDS within the section, or to the start
 * of the sentence. While paused, a voice that holds its place moves it and
 * stays paused; a paused device voice is already at its sentence's start,
 * so nothing changes. */
export function readAloudBack(): void {
  if (!movable()) return;
  if (playback?.seekBack) {
    playback.seekBack(READ_ALOUD_BACK_SECONDS);
    return;
  }
  if (player.phase === 'playing') {
    stopVoice();
    speakFrom(player.index);
  }
}

function currentSection(): number {
  return layout.sectionOf[player.index] ?? 0;
}

// Play from `piece` and end in 'playing': through the playback when it can
// skip, otherwise by speaking again from there. Called from a click or a
// key, so a new clip playback starts inside the gesture.
function jumpTo(piece: number): void {
  if (playback?.skipTo) {
    player.index = piece;
    player.phase = 'playing';
    player.waiting = player.route?.kind === 'openai';
    playback.skipTo(piece);
    return;
  }
  stopVoice();
  speakFrom(piece);
}

// Whether the reading in progress can be moved or resumed: something is
// being read or paused and its route can still serve it.
function movable(): boolean {
  return player.phase !== 'idle' && player.route !== null && player.owner !== null && servable(player.route, player.owner);
}

function resumable(): boolean {
  return player.phase === 'paused' && movable();
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
  current = speaker.speak(layout.pieces, index, { voiceId, rate: prefs.rate, volume: prefs.volume }, (at) => {
    if (current !== null && playback !== current) return;
    player.index = at;
    player.waiting = false;
  });
  playback = current;
  current.done.then(
    () => {
      // Stopping drops the handle before it stops the voice, so a current
      // playback that ends, paused or not, finished or stopped from
      // outside (another page cancelling speech), is over.
      if (playback !== current) return;
      playback = null;
      reset();
    },
    (err: unknown) => {
      if (playback !== current) return;
      playback = null;
      reset();
      if (route.kind === 'device') addErrorToast('Read aloud stopped: this device’s voice failed.', err);
      // The computer's switch went off or was written there on purpose: the
      // reading stops quietly and the picker and controls catch up.
      else if (isReadAloudSwitchedOff(err)) void refreshReadAloudSource(route.backend);
      else addErrorToast(`Read aloud stopped: ${err instanceof Error ? err.message : String(err)}`, err);
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
  player.waiting = false;
  player.route = null;
  player.owner = null;
  layout = EMPTY_LAYOUT;
}

// Turning read-aloud off, the device losing its voices, or an OpenAI
// reading's computer no longer offering its voice or no longer owning its
// text stops a reading in progress and forgets its place.
$effect.root(() => {
  $effect(() => {
    const route = player.route;
    const owner = player.owner;
    if (route && owner && !servable(route, owner)) untrack(stopReadAloud);
  });
});

/** Forget everything, including the engine. Tests only. */
export function resetReadAloudForTest(): void {
  stopReadAloud();
  engine = null;
  watchingVoices = false;
  voices.list = [];
  openAIEngineFor = clipEngineFor;
}

/** Read OpenAI voices through `factory`'s engine. Tests only. */
export function useOpenAIEngineForTest(factory: (backend: BackendKey) => SpeechEngine): void {
  openAIEngineFor = factory;
}
