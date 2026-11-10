// One composer's dictation: record from the microphone, upload the
// recording in pieces to the computer the composer sends to while it
// records, and hand the transcript back when the user stops.
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
  setInterval(fn: () => void, ms: number): unknown;
  clearInterval(handle: unknown): void;
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

export interface DictationRemote {
  begin(container: string): Promise<string>;
  upload(recordingId: string, seq: number, piece: Blob): Promise<void>;
  finish(recordingId: string, pieces: number): Promise<string>;
  cancel(recordingId: string): Promise<void>;
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

export class DictationSession {
  phase = $state<DictationPhase>('idle');
  /** Stepped loudness while recording, 0..LEVEL_STEPS. */
  level = $state(0);
  /** Why the last attempt produced nothing, until the next one starts. */
  error = $state<string | null>(null);

  private readonly env: DictationEnvironment;
  private readonly remote: DictationRemote;
  private readonly onTranscript: (text: string) => boolean;
  /** The attempt in progress. A superseded attempt checks this after every
   * await and changes nothing of the session's once it is not current. */
  private current: Attempt | null = null;

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
    const format = this.env.format();
    if (format === null) {
      this.error = 'This browser cannot record audio.';
      return;
    }
    const attempt = new Attempt(this.env, this.remote);
    this.current = attempt;
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
      // Whatever this attempt opened before failing, here or on the
      // computer, closes with it.
      attempt.abandon();
      if (this.current !== attempt) return;
      this.current = null;
      this.phase = 'idle';
      this.level = 0;
      this.error = microphoneError(err, this.env.blockedMessage());
    }
  }

  private sampleLevel(attempt: Attempt): void {
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
    await attempt.uploads;
    if (this.current !== attempt) return;
    const refusal = attempt.refusal();
    if (refusal !== null) {
      attempt.abandon();
      this.settle(attempt, refusal);
      return;
    }
    try {
      const text = await this.remote.finish(attempt.recordingId!, attempt.pieces);
      attempt.finished();
      if (this.current !== attempt) return;
      if (text.trim() === '') {
        this.settle(attempt, 'No speech heard.');
        return;
      }
      this.settle(attempt, this.onTranscript(text) ? null : 'The transcript could not be inserted.');
    } catch (err) {
      attempt.finished();
      if (this.current !== attempt) return;
      this.settle(attempt, errorMessage(err, 'Transcription failed.'));
    }
  }

  private settle(attempt: Attempt, error: string | null): void {
    if (this.current !== attempt) return;
    this.current = null;
    this.phase = 'idle';
    this.level = 0;
    this.error = error;
  }

  /** Abandon the attempt: nothing is transcribed or inserted, and the
   * computer drops the recording or stops its transcription. */
  cancel(): void {
    const attempt = this.current;
    if (attempt === null) return;
    this.current = null;
    attempt.abandon();
    this.phase = 'idle';
    this.level = 0;
  }

  /** Release the microphone for good; the owner is unmounting. */
  dispose(): void {
    this.cancel();
  }
}

/** One recording attempt and everything it holds. Its methods touch only
 * its own resources, so a superseded attempt cannot release a newer one's. */
class Attempt {
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
