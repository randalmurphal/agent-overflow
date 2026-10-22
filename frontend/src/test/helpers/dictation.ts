// A DictationSession over a scripted microphone, recorder and computer, for
// tests of the session and the controls that drive it.

import { vi } from 'vitest';
import {
  DictationSession,
  type DictationEnvironment,
  type DictationSourceKind,
  type PcmStream,
  type RecorderLike,
  type RecordingFormat,
} from '../../lib/dictation/dictationSession.svelte';

const WEBM: RecordingFormat = { container: 'audio/webm', recorderType: 'audio/webm;codecs=opus', timeslice: 1000 };

/** The fake computer's refusal of a recording that switching live off
 * ended; endedBySwitch recognizes it. */
export const SWITCHED_OFF = 'Live dictation was switched off, so this recording ended.';

export class FakeRecorder implements RecorderLike {
  state: 'inactive' | 'recording' | 'paused' = 'inactive';
  ondataavailable: ((event: { data: Blob }) => void) | null = null;
  onstop: (() => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  timeslice: number | undefined;
  /** Pieces still buffered, delivered at stop as the engine flushes. */
  tail: Blob[] = [new Blob(['tail'])];

  start(timeslice?: number): void {
    this.timeslice = timeslice;
    this.state = 'recording';
  }
  emit(text: string): void {
    this.ondataavailable?.({ data: new Blob([text]) });
  }
  stop(): void {
    this.state = 'inactive';
    for (const piece of this.tail) this.ondataavailable?.({ data: piece });
    queueMicrotask(() => this.onstop?.());
  }
}

/** A live capture whose pieces the test emits. */
export class FakePcm implements PcmStream {
  onPiece: ((pcm: Uint8Array) => void) | null = null;
  closed = false;
  /** Bytes delivered at flush, as the worklet posts its part-filled piece. */
  tail: Uint8Array | null = new Uint8Array([9, 9]);
  /** Makes flush fail, as a capture that never answers does. */
  flushFails = false;

  emit(...bytes: number[]): void {
    this.onPiece?.(new Uint8Array(bytes));
  }
  async flush(): Promise<void> {
    if (this.flushFails) throw new Error('The microphone capture did not stop in time.');
    if (this.tail !== null) this.onPiece?.(this.tail);
  }
  close(): void {
    this.closed = true;
  }
}

type Mocked<T> = { [K in keyof T]: ReturnType<typeof vi.fn> };

export interface DictationFixture {
  session: DictationSession;
  env: DictationEnvironment;
  remote: {
    source: ReturnType<typeof vi.fn>;
    begin: ReturnType<typeof vi.fn>;
    upload: ReturnType<typeof vi.fn>;
    finish: ReturnType<typeof vi.fn>;
    cancel: ReturnType<typeof vi.fn>;
    live: Mocked<{ begin: unknown; append: unknown; finish: unknown; cancel: unknown; stillOn: unknown; endedBySwitch: unknown }>;
  };
  recorder: () => FakeRecorder;
  /** The live capture the session opened last. */
  pcm: () => FakePcm;
  tracks: { stop: ReturnType<typeof vi.fn> }[];
  inserted: string[];
  level: { value: number; closed: boolean };
  /** How many times the session quieted this device's speech. */
  quieted: { count: number };
  tick: () => void;
  fireCap: () => void;
  timers: () => number;
}

export function dictationFixture(options: {
  format?: RecordingFormat | null;
  openMicrophone?: () => Promise<MediaStream>;
  meter?: boolean;
  insert?: boolean;
  /** Replaces the recorder the environment builds, e.g. to make it throw. */
  createRecorder?: () => FakeRecorder;
  /** The source the computer chooses; recorded by default. */
  source?: DictationSourceKind;
  streamsPcm?: boolean;
  openPcm?: () => Promise<FakePcm>;
} = {}): DictationFixture {
  const tracks: { stop: ReturnType<typeof vi.fn> }[] = [];
  let current: FakeRecorder | null = null;
  let currentPcm: FakePcm | null = null;
  const level = { value: 0.5, closed: false };
  const quieted = { count: 0 };
  const intervals = new Map<number, () => void>();
  const timeouts = new Map<number, () => void>();
  let nextHandle = 1;
  const env: DictationEnvironment = {
    format: () => (options.format === undefined ? WEBM : options.format),
    blockedMessage: () => 'Microphone access is blocked.',
    quietSpeech: () => { quieted.count += 1; },
    openMicrophone: options.openMicrophone ?? (async () => {
      const track = { stop: vi.fn() };
      tracks.push(track);
      return { getTracks: () => [track] } as unknown as MediaStream;
    }),
    createRecorder: () => (current = options.createRecorder?.() ?? new FakeRecorder()),
    streamsPcm: () => options.streamsPcm ?? true,
    openPcm: async () => (currentPcm = options.openPcm ? await options.openPcm() : new FakePcm()),
    createLevelMeter: () => options.meter === false ? null : {
      read: () => level.value,
      close: () => { level.closed = true; },
    },
    setInterval: (fn) => { const h = nextHandle++; intervals.set(h, fn); return h; },
    clearInterval: (h) => { intervals.delete(h as number); },
    setTimeout: (fn) => { const h = nextHandle++; timeouts.set(h, fn); return h; },
    clearTimeout: (h) => { timeouts.delete(h as number); },
  };
  const remote = {
    source: vi.fn(() => options.source ?? 'recorded'),
    begin: vi.fn(async () => 'rec-1'),
    upload: vi.fn(async () => {}),
    finish: vi.fn(async () => 'hello world'),
    cancel: vi.fn(async () => {}),
    live: {
      begin: vi.fn(async () => 'live-1'),
      append: vi.fn(async (_id: string, seq: number) => `heard ${seq}`),
      finish: vi.fn(async () => 'hello live'),
      cancel: vi.fn(async () => {}),
      stillOn: vi.fn(async () => true),
      endedBySwitch: vi.fn((err: unknown) => err instanceof Error && err.message === SWITCHED_OFF),
    },
  };
  const inserted: string[] = [];
  const session = new DictationSession(env, remote, (text) => {
    if (options.insert === false) return false;
    inserted.push(text);
    return true;
  });
  return {
    session,
    env,
    remote,
    recorder: () => current!,
    pcm: () => currentPcm!,
    tracks,
    inserted,
    level,
    quieted,
    tick: () => { for (const fn of intervals.values()) fn(); },
    fireCap: () => { for (const fn of [...timeouts.values()]) fn(); },
    timers: () => intervals.size + timeouts.size,
  };
}
