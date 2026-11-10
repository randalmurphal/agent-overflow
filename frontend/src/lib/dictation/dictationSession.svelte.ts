// One composer's dictation: record from the microphone, upload the
// recording in pieces to the computer the composer sends to while it
// records, and hand the transcript back when the user stops. With the
// computer's live source the pieces are raw PCM it relays to OpenAI as
// they arrive, and the text heard so far shows while the user speaks.
//
// Browser APIs come in through DictationEnvironment and the computer's
// calls through DictationRemote, so the transitions are testable without a
// microphone. The session never sends anything: the transcript goes to
// onTranscript, which inserts it at the caret.

export type DictationPhase = 'idle' | 'starting' | 'recording' | 'transcribing';

/** How this engine's MediaRecorder records: the container the computer
 * accepts, the exact recorder type, and whether pieces stream. */
export interface RecordingFormat {
  /** audio/webm or audio/mp4, what BeginDictation takes. */
  container: string;
  /** The MediaRecorder mimeType, codecs included. */
  recorderType: string;
  /** Milliseconds between pieces, or undefined for one piece at stop.
   * WebKit's MP4 pieces are not independently framed, so it records
   * whole and uploads once. */
  timeslice: number | undefined;
}

export interface RecorderLike {
  start(timeslice?: number): void;
  stop(): void;
  readonly state: 'inactive' | 'recording' | 'paused';
  ondataavailable: ((event: { data: Blob }) => void) | null;
  onstop: (() => void) | null;
  onerror: ((event: unknown) => void) | null;
}

/** Live capture: 24 kHz 16-bit PCM pieces as the microphone hears them. */
export interface PcmStream {
  onPiece: ((pcm: Uint8Array) => void) | null;
  /** Delivers the part-filled last piece, then resolves. */
  flush(): Promise<void>;
  close(): void;
}

export interface LevelMeter {
  /** Current loudness, 0 to 1. */
  read(): number;
  close(): void;
}

export interface DictationEnvironment {
  format(): RecordingFormat | null;
  /** What to say when the microphone is refused: where to allow it here. */
  blockedMessage(): string;
  /** Quiet this device's own speech so the microphone does not hear it. */
  quietSpeech(): void;
  openMicrophone(): Promise<MediaStream>;
  createRecorder(stream: MediaStream, format: RecordingFormat): RecorderLike;
  createLevelMeter(stream: MediaStream): LevelMeter | null;
  /** Whether this engine can stream PCM for live dictation. */
  streamsPcm(): boolean;
  openPcm(stream: MediaStream): Promise<PcmStream>;
  setInterval(fn: () => void, ms: number): unknown;
  clearInterval(handle: unknown): void;
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

/** How the next recording is transcribed: recorded and uploaded, then
 * transcribed whole, or streamed live. */
export type DictationSourceKind = 'recorded' | 'live';

export interface LiveDictationRemote {
  begin(): Promise<string>;
  /** Sends piece seq and answers with the text heard so far. */
  append(recordingId: string, seq: number, pcm: Uint8Array): Promise<string>;
  finish(recordingId: string): Promise<string>;
  cancel(recordingId: string): Promise<void>;
  /** Asks the computer again whether live dictation is on. */
  stillOn(): Promise<boolean>;
  /** Whether err is the computer saying live was switched off since the
   * recording began, which ended it. */
  endedBySwitch(err: unknown): boolean;
}

export interface DictationRemote {
  /** The source the computer and this device choose for the next
   * recording. */
  source(): DictationSourceKind;
  begin(container: string): Promise<string>;
  upload(recordingId: string, seq: number, piece: Blob): Promise<void>;
  finish(recordingId: string, pieces: number): Promise<string>;
  cancel(recordingId: string): Promise<void>;
  live: LiveDictationRemote;
}

/** A recording stops itself after this long; the computer caps the bytes. */
export const MAX_RECORDING_MS = 10 * 60 * 1000;

/** Below this peak loudness nothing was said, and nothing is uploaded
 * for transcription. */
export const SILENCE_PEAK = 0.02;

/** The level shown steps at this rate rather than per frame. */
const LEVEL_INTERVAL_MS = 100;

/** Shown levels: 0 to LEVEL_STEPS. */
export const LEVEL_STEPS = 4;

/** The most PCM one live append carries: five seconds, the computer's
 * cap. Pieces that pile up while an append is in flight go together. */
export const MAX_LIVE_APPEND_BYTES = 5 * 24_000 * 2;

export class DictationSession {
  phase = $state<DictationPhase>('idle');
  /** Stepped loudness while recording, 0..LEVEL_STEPS. */
  level = $state(0);
  /** Why the last attempt produced nothing, until the next one starts. */
  error = $state<string | null>(null);
  /** What a live recording has heard so far; empty otherwise. */
  heard = $state('');
  /** The source the attempt in progress transcribes with, null when idle.
   * The owner cancels it when that source goes away, even if another
   * remains. */
  activeSource = $state<DictationSourceKind | null>(null);

  private readonly env: DictationEnvironment;
  private readonly remote: DictationRemote;
  private readonly onTranscript: (text: string) => boolean;
  /** The attempt in progress. A superseded attempt checks this after every
   * await and changes nothing of the session's once it is not current. */
  private current: Recording | null = null;

  /** `onTranscript` inserts the text and answers whether it landed. */
  constructor(env: DictationEnvironment, remote: DictationRemote, onTranscript: (text: string) => boolean) {
    this.env = env;
    this.remote = remote;
    this.onTranscript = onTranscript;
  }

  /** Start when idle, stop when recording; ignored in between. */
  toggle(): void {
    if (this.phase === 'idle') void this.start();
    else if (this.phase === 'recording') void this.stop();
  }

  async start(): Promise<void> {
    if (this.phase !== 'idle') return;
    this.error = null;
    this.heard = '';
    if (this.remote.source() === 'live') {
      await this.startLive();
      return;
    }
    const format = this.env.format();
    if (format === null) {
      this.error = 'This browser cannot record audio.';
      return;
    }
    const attempt = new Attempt(this.env, this.remote);
    this.current = attempt;
    this.activeSource = 'recorded';
    this.phase = 'starting';
    this.env.quietSpeech();
    try {
      attempt.stream = await this.env.openMicrophone();
      if (this.current !== attempt) {
        attempt.abandon();
        return;
      }
      attempt.recordingId = await this.remote.begin(format.container);
      if (this.current !== attempt) {
        attempt.abandon();
        return;
      }
      attempt.record(format, () => this.sampleLevel(attempt), () => void this.stop());
      this.phase = 'recording';
    } catch (err) {
      this.failStart(attempt, err);
    }
  }

  /** The live start: speech is captured from the moment the microphone
   * opens and queued until the computer has connected to OpenAI. */
  private async startLive(): Promise<void> {
    if (!this.env.streamsPcm()) {
      this.error = 'This browser cannot stream audio for live dictation.';
      return;
    }
    const attempt: LiveAttempt = new LiveAttempt(
      this.env,
      this.remote.live,
      (heard) => {
        if (this.current === attempt) this.heard = heard;
      },
      (err) => {
        // The connect or an append failed: stop now, not when the user
        // finishes speaking into nothing.
        attempt.abandon();
        this.settleLiveFailure(attempt, err);
      },
    );
    this.current = attempt;
    this.activeSource = 'live';
    this.phase = 'starting';
    this.env.quietSpeech();
    try {
      attempt.stream = await this.env.openMicrophone();
      if (this.current !== attempt) {
        attempt.abandon();
        return;
      }
      const pcm = await this.env.openPcm(attempt.stream);
      if (this.current !== attempt) {
        // Abandoned while the capture opened: it was never the attempt's
        // to release, so close it here.
        pcm.close();
        attempt.abandon();
        return;
      }
      attempt.capture(pcm, () => this.sampleLevel(attempt), () => void this.stop());
      // Recording starts now; the computer connects to OpenAI meanwhile.
      attempt.connect(this.remote.live.begin());
      this.phase = 'recording';
    } catch (err) {
      this.failStart(attempt, err);
    }
  }

  private failStart(attempt: Recording, err: unknown): void {
    // Whatever this attempt opened before failing, here or on the
    // computer, closes with it.
    attempt.abandon();
    if (this.current !== attempt) return;
    this.current = null;
    this.activeSource = null;
    this.phase = 'idle';
    this.level = 0;
    this.heard = '';
    this.error = microphoneError(err, this.env.blockedMessage());
  }

  private sampleLevel(attempt: Recording): void {
    if (this.current !== attempt) return;
    const value = attempt.sample();
    this.level = Math.min(LEVEL_STEPS, Math.round(Math.min(1, value * 8) * LEVEL_STEPS));
  }

  /** Stop recording and transcribe. The transcript goes to onTranscript. */
  async stop(): Promise<void> {
    const attempt = this.current;
    if (this.phase !== 'recording' || attempt === null) return;
    this.sampleLevel(attempt);
    this.phase = 'transcribing';
    this.level = 0;
    // The microphone is released as soon as the recorder has its last
    // piece, before any network wait.
    await attempt.stopRecording();
    if (this.current !== attempt) return;
    await attempt.delivered();
    // A live failure settles through its own path, which first asks
    // whether live was switched off.
    if (this.current !== attempt || attempt.failed()) return;
    const refusal = attempt.refusal();
    if (refusal !== null) {
      attempt.abandon();
      this.settle(attempt, refusal);
      return;
    }
    try {
      const text = await attempt.transcribe();
      attempt.finished();
      if (this.current !== attempt) return;
      if (text.trim() === '') {
        this.settle(attempt, attempt.lostEnding() ?? 'No speech heard.');
        return;
      }
      // What was heard lands even when the capture lost its ending; the
      // mic says so.
      this.settle(attempt, this.onTranscript(text) ? attempt.lostEnding() : 'The transcript could not be inserted.');
    } catch (err) {
      attempt.finished();
      if (this.current !== attempt) return;
      if (attempt instanceof LiveAttempt) this.settleLiveFailure(attempt, err);
      else this.settle(attempt, errorMessage(err, 'Transcription failed.'));
    }
  }

  /** A live failure because live was switched off is that switch's stop,
   * not an error to show. The computer says so when it ended the
   * recording; otherwise it is asked whether live is still on, since the
   * failure may have raced the switch. */
  private settleLiveFailure(attempt: Recording, err: unknown): void {
    if (this.remote.live.endedBySwitch(err)) {
      this.settle(attempt, null);
      return;
    }
    const failed = errorMessage(err, 'Live transcription failed.');
    void this.remote.live.stillOn().then(
      (on) => this.settle(attempt, on ? failed : null),
      () => this.settle(attempt, failed),
    );
  }

  private settle(attempt: Recording, error: string | null): void {
    if (this.current !== attempt) return;
    this.current = null;
    this.activeSource = null;
    this.phase = 'idle';
    this.level = 0;
    this.heard = '';
    this.error = error;
  }

  /** Abandon the attempt: nothing is transcribed or inserted, and the
   * computer drops the recording or stops its transcription. */
  cancel(): void {
    const attempt = this.current;
    if (attempt === null) return;
    this.current = null;
    this.activeSource = null;
    attempt.abandon();
    this.phase = 'idle';
    this.level = 0;
    this.heard = '';
  }

  /** Release the microphone for good; the owner is unmounting. */
  dispose(): void {
    this.cancel();
  }
}

/** What the session asks of an attempt, recorded or live. Its methods
 * touch only its own resources, so a superseded attempt cannot release a
 * newer one's. */
interface Recording {
  stream: MediaStream | null;
  recordingId: string | null;
  sample(): number;
  /** Stops capture, keeps its last audio, and releases the microphone. */
  stopRecording(): Promise<void>;
  /** Resolves once everything captured has reached the computer. */
  delivered(): Promise<void>;
  /** Whether the attempt failed and is settling itself. */
  failed(): boolean;
  /** Why the recording should not be transcribed, or null. */
  refusal(): string | null;
  /** Why the transcript may miss the end of what was said, or null. */
  lostEnding(): string | null;
  transcribe(): Promise<string>;
  finished(): void;
  abandon(): void;
}

/** One recorded attempt and everything it holds. */
class Attempt implements Recording {
  stream: MediaStream | null = null;
  recordingId: string | null = null;
  pieces = 0;
  uploads: Promise<void> = Promise.resolve();
  private readonly env: DictationEnvironment;
  private readonly remote: DictationRemote;
  private recorder: RecorderLike | null = null;
  private meter: LevelMeter | null = null;
  private levelTimer: unknown = null;
  private capTimer: unknown = null;
  private stopped: Promise<void> = Promise.resolve();
  private peak = 0;
  private metered = false;
  private uploadError: unknown = null;
  private abandoned = false;
  /** The computer has let the recording go: finished, or told to cancel. */
  private released = false;

  constructor(env: DictationEnvironment, remote: DictationRemote) {
    this.env = env;
    this.remote = remote;
  }

  record(format: RecordingFormat, onLevel: () => void, onCap: () => void): void {
    const stream = this.stream!;
    const recordingId = this.recordingId!;
    const recorder = this.env.createRecorder(stream, format);
    this.recorder = recorder;
    recorder.ondataavailable = ({ data }) => {
      if (this.abandoned || data.size === 0) return;
      const seq = this.pieces++;
      // One at a time and in order: the computer refuses a piece out of
      // order. A failure stops later pieces from being sent at all.
      this.uploads = this.uploads.then(async () => {
        if (this.abandoned || this.uploadError !== null) return;
        try {
          await this.remote.upload(recordingId, seq, data);
        } catch (err) {
          if (!this.abandoned) this.uploadError = err;
        }
      });
    };
    this.stopped = new Promise((resolve) => {
      recorder.onstop = () => resolve();
    });
    recorder.onerror = () => {
      if (!this.abandoned) this.uploadError = new Error('The recorder stopped unexpectedly.');
    };
    this.meter = this.env.createLevelMeter(stream);
    this.metered = this.meter !== null;
    this.levelTimer = this.env.setInterval(onLevel, LEVEL_INTERVAL_MS);
    this.capTimer = this.env.setTimeout(onCap, MAX_RECORDING_MS);
    recorder.start(format.timeslice);
  }

  sample(): number {
    const value = this.meter?.read() ?? 0;
    if (value > this.peak) this.peak = value;
    return value;
  }

  delivered(): Promise<void> {
    return this.uploads;
  }

  transcribe(): Promise<string> {
    return this.remote.finish(this.recordingId!, this.pieces);
  }

  /** Stops the recorder, waits for its last piece, and releases the
   * microphone. */
  async stopRecording(): Promise<void> {
    this.stopTimers();
    const recorder = this.recorder;
    if (recorder && recorder.state !== 'inactive') recorder.stop();
    await this.stopped;
    this.releaseDevices();
  }

  /** Why the recording should not be transcribed, or null to transcribe. */
  refusal(): string | null {
    if (this.uploadError !== null) return errorMessage(this.uploadError, 'Recording upload failed.');
    // Without a meter there is no silence check; the computer decides.
    if ((this.metered && this.peak < SILENCE_PEAK) || this.pieces === 0) return 'No speech heard.';
    return null;
  }

  /** An upload failure is this attempt's refusal, settled by stop. */
  failed(): boolean {
    return false;
  }

  /** The recorder hands over its last piece before it stops. */
  lostEnding(): string | null {
    return null;
  }

  finished(): void {
    this.released = true;
  }

  /** Releases everything the attempt holds and tells the computer to drop
   * the recording or stop transcribing it. Safe to call more than once. */
  abandon(): void {
    this.abandoned = true;
    this.stopTimers();
    const recorder = this.recorder;
    if (recorder && recorder.state !== 'inactive') recorder.stop();
    this.releaseDevices();
    const recordingId = this.recordingId;
    if (recordingId !== null && !this.released) {
      this.released = true;
      void this.remote.cancel(recordingId).catch(() => {});
    }
  }

  private stopTimers(): void {
    if (this.levelTimer !== null) this.env.clearInterval(this.levelTimer);
    if (this.capTimer !== null) this.env.clearTimeout(this.capTimer);
    this.levelTimer = null;
    this.capTimer = null;
  }

  private releaseDevices(): void {
    this.meter?.close();
    this.meter = null;
    if (this.stream) stopStream(this.stream);
    this.stream = null;
    if (this.recorder) {
      this.recorder.ondataavailable = null;
      this.recorder.onerror = null;
    }
    this.recorder = null;
  }
}

/** One live attempt: PCM captured from the moment the microphone opens,
 * queued until the computer has connected, then sent in order, one append
 * in flight at a time. */
class LiveAttempt implements Recording {
  stream: MediaStream | null = null;
  recordingId: string | null = null;
  private readonly env: DictationEnvironment;
  private readonly remote: LiveDictationRemote;
  private readonly onHeard: (heard: string) => void;
  private readonly onFailed: (err: unknown) => void;
  private connecting: Promise<void> = Promise.resolve();
  private pcm: PcmStream | null = null;
  private meter: LevelMeter | null = null;
  private levelTimer: unknown = null;
  private capTimer: unknown = null;
  /** The loudest sample sent so far, 0 to 1. */
  private peak = 0;
  private queue: Uint8Array[] = [];
  private sending: Promise<void> | null = null;
  private seq = 0;
  private sendError: unknown = null;
  private flushError: unknown = null;
  private abandoned = false;
  private released = false;

  constructor(
    env: DictationEnvironment,
    remote: LiveDictationRemote,
    onHeard: (heard: string) => void,
    onFailed: (err: unknown) => void,
  ) {
    this.env = env;
    this.remote = remote;
    this.onHeard = onHeard;
    this.onFailed = onFailed;
  }

  capture(pcm: PcmStream, onLevel: () => void, onCap: () => void): void {
    if (this.abandoned || this.stream === null) {
      pcm.close();
      return;
    }
    this.pcm = pcm;
    pcm.onPiece = (piece) => {
      if (this.abandoned || this.sendError !== null) return;
      this.hearPeak(piece);
      this.queue.push(piece);
      this.pump();
    };
    // The meter only drives the ring here; silence is judged from every
    // sample sent, which a sampled meter can miss between reads.
    this.meter = this.env.createLevelMeter(this.stream!);
    this.levelTimer = this.env.setInterval(onLevel, LEVEL_INTERVAL_MS);
    this.capTimer = this.env.setTimeout(onCap, MAX_RECORDING_MS);
  }

  /** The computer is connecting; once it has, what was queued meanwhile
   * is sent. An attempt abandoned while it connected drops the session. */
  connect(begin: Promise<string>): void {
    this.connecting = begin.then(
      (recordingId) => {
        this.recordingId = recordingId;
        if (this.abandoned) {
          this.released = true;
          void this.remote.cancel(recordingId).catch(() => {});
          return;
        }
        this.pump();
      },
      (err) => this.fail(err),
    );
  }

  private fail(err: unknown): void {
    if (this.abandoned || this.sendError !== null) return;
    this.sendError = err;
    this.onFailed(err);
  }

  private pump(): void {
    const recordingId = this.recordingId;
    if (recordingId === null || this.sending !== null || this.sendError !== null || this.abandoned || this.queue.length === 0) return;
    const piece = this.take();
    const seq = this.seq++;
    this.sending = this.remote.append(recordingId, seq, piece).then(
      (heard) => {
        if (!this.abandoned) this.onHeard(heard);
      },
      (err) => this.fail(err),
    ).finally(() => {
      this.sending = null;
      this.pump();
    });
  }

  /** The queued pieces, joined, up to one append's worth. */
  private take(): Uint8Array {
    let size = 0;
    let count = 0;
    while (count < this.queue.length && size + this.queue[count].length <= MAX_LIVE_APPEND_BYTES) {
      size += this.queue[count].length;
      count += 1;
    }
    if (count === 0) count = 1;
    const pieces = this.queue.splice(0, count);
    if (pieces.length === 1) return pieces[0];
    const joined = new Uint8Array(size);
    let offset = 0;
    for (const piece of pieces) {
      joined.set(piece, offset);
      offset += piece.length;
    }
    return joined;
  }

  sample(): number {
    return this.meter?.read() ?? 0;
  }

  /** The loudest sample of a little-endian 16-bit piece, as 0 to 1. */
  private hearPeak(piece: Uint8Array): void {
    const samples = new DataView(piece.buffer, piece.byteOffset, piece.byteLength);
    let loudest = this.peak * 0x8000;
    for (let offset = 0; offset + 1 < piece.byteLength; offset += 2) {
      const sample = Math.abs(samples.getInt16(offset, true));
      if (sample > loudest) loudest = sample;
    }
    this.peak = loudest / 0x8000;
  }

  async stopRecording(): Promise<void> {
    this.stopTimers();
    const pcm = this.pcm;
    if (pcm !== null) {
      try {
        await pcm.flush();
      } catch (err) {
        // What it sent stands; the mic says the ending may be missing.
        this.flushError = err;
      }
    }
    this.releaseDevices();
  }

  async delivered(): Promise<void> {
    await this.connecting;
    while (this.sending !== null) await this.sending;
  }

  failed(): boolean {
    return this.sendError !== null;
  }

  refusal(): string | null {
    if (this.peak < SILENCE_PEAK || this.seq === 0) return 'No speech heard.';
    return null;
  }

  lostEnding(): string | null {
    if (this.flushError === null) return null;
    return `The end of the recording may be missing: ${errorMessage(this.flushError, 'the microphone capture did not stop cleanly.')}`;
  }

  transcribe(): Promise<string> {
    return this.remote.finish(this.recordingId!);
  }

  finished(): void {
    this.released = true;
  }

  abandon(): void {
    this.abandoned = true;
    this.queue = [];
    this.stopTimers();
    this.releaseDevices();
    const recordingId = this.recordingId;
    if (recordingId !== null && !this.released) {
      this.released = true;
      void this.remote.cancel(recordingId).catch(() => {});
    }
  }

  private stopTimers(): void {
    if (this.levelTimer !== null) this.env.clearInterval(this.levelTimer);
    if (this.capTimer !== null) this.env.clearTimeout(this.capTimer);
    this.levelTimer = null;
    this.capTimer = null;
  }

  private releaseDevices(): void {
    this.meter?.close();
    this.meter = null;
    if (this.pcm !== null) {
      this.pcm.onPiece = null;
      this.pcm.close();
    }
    this.pcm = null;
    if (this.stream) stopStream(this.stream);
    this.stream = null;
  }
}

function stopStream(stream: MediaStream): void {
  for (const track of stream.getTracks()) track.stop();
}

function microphoneError(err: unknown, blocked: string): string {
  const name = err instanceof Error || (typeof err === 'object' && err !== null && 'name' in err)
    ? String((err as { name?: unknown }).name)
    : '';
  if (name === 'NotAllowedError' || name === 'SecurityError') return blocked;
  if (name === 'NotFoundError' || name === 'OverconstrainedError') return 'No microphone found.';
  if (name === 'NotReadableError') return 'The microphone is in use by another app.';
  return errorMessage(err, 'Could not start recording.');
}

function errorMessage(err: unknown, fallback: string): string {
  if (err instanceof Error && err.message.trim() !== '') return err.message;
  return fallback;
}
