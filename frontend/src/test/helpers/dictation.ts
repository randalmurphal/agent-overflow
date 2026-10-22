// A DictationSession over a scripted microphone, recorder and computer, for
// tests of the session and the controls that drive it.

import { vi } from 'vitest';
import {
  DictationSession,
  type DictationEnvironment,
  type DictationRemote,
  type RecorderLike,
  type RecordingFormat,
} from '../../lib/dictation/dictationSession.svelte';

const WEBM: RecordingFormat = { container: 'audio/webm', recorderType: 'audio/webm;codecs=opus', timeslice: 1000 };

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

export interface DictationFixture {
  session: DictationSession;
  env: DictationEnvironment;
  remote: { [K in keyof DictationRemote]: ReturnType<typeof vi.fn> };
  recorder: () => FakeRecorder;
  tracks: { stop: ReturnType<typeof vi.fn> }[];
  inserted: string[];
  level: { value: number; closed: boolean };
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
} = {}): DictationFixture {
  const tracks: { stop: ReturnType<typeof vi.fn> }[] = [];
  let current: FakeRecorder | null = null;
  const level = { value: 0.5, closed: false };
  const intervals = new Map<number, () => void>();
  const timeouts = new Map<number, () => void>();
  let nextHandle = 1;
  const env: DictationEnvironment = {
    format: () => (options.format === undefined ? WEBM : options.format),
    blockedMessage: () => 'Microphone access is blocked.',
    openMicrophone: options.openMicrophone ?? (async () => {
      const track = { stop: vi.fn() };
      tracks.push(track);
      return { getTracks: () => [track] } as unknown as MediaStream;
    }),
    createRecorder: () => (current = options.createRecorder?.() ?? new FakeRecorder()),
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
    begin: vi.fn(async () => 'rec-1'),
    upload: vi.fn(async () => {}),
    finish: vi.fn(async () => 'hello world'),
    cancel: vi.fn(async () => {}),
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
    tracks,
    inserted,
    level,
    tick: () => { for (const fn of intervals.values()) fn(); },
    fireCap: () => { for (const fn of [...timeouts.values()]) fn(); },
    timers: () => intervals.size + timeouts.size,
  };
}
