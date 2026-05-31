/**
 * Frontend-facing types for the terminal subsystem. These mirror the Go
 * structs in internal/terminal/session.go and app_terminal.go but remain
 * decoupled from the generated Wails bindings so UI code doesn't import
 * anything heavier than needed.
 */

export interface TerminalSessionSummary {
  terminalID: string;
  threadID: string;
  shell: string;
  cwd: string;
  rows: number;
  cols: number;
  pid: number;
  startedAt: number;
  running: boolean;
  exitCode: number;
  exitReason: string;
}

export interface TerminalHandle {
  terminalID: string;
  threadID: string;
  summary: TerminalSessionSummary;
}

export interface TerminalReplay {
  data: string;
  fromSequence: number;
  throughSequence: number;
}

export interface TerminalOpenOptions {
  cwd: string;
  shell?: string;
  rows?: number;
  cols?: number;
}

export interface TerminalOutputEventPayload {
  terminalID: string;
  threadID: string;
  sequence: number;
  /**
   * Base64-encoded bytes. Terminal output can contain non-UTF-8 escape
   * sequences, so we keep the wire format binary-safe.
   */
  data: string;
}

export interface TerminalExitEventPayload {
  terminalID: string;
  threadID: string;
  code: number;
  reason: string;
}

export function normalizeTerminalReplay(value: unknown): TerminalReplay {
  if (!value) return { data: '', fromSequence: 0, throughSequence: 0 };
  if (typeof value === 'string') return { data: value, fromSequence: 0, throughSequence: 0 };
  if (typeof value !== 'object') return { data: '', fromSequence: 0, throughSequence: 0 };
  const replay = value as Partial<TerminalReplay>;
  return {
    data: replay.data ?? '',
    fromSequence: Number.isFinite(replay.fromSequence) ? replay.fromSequence ?? 0 : 0,
    throughSequence: Number.isFinite(replay.throughSequence) ? replay.throughSequence ?? 0 : 0,
  };
}

/**
 * Decode a base64 output chunk into the raw PTY bytes. xterm's `write()`
 * treats a `Uint8Array` as UTF-8 and its decoder is stream-aware, so a
 * multi-byte character split across two PTY read chunks still composes
 * correctly. Decoding to a string instead would make xterm interpret each
 * byte value as a UTF-16 code unit, mojibaking every non-ASCII glyph
 * (box-drawing, `·`, emoji) — the classic TUI-renders-as-garbage bug.
 */
export function decodeTerminalOutput(dataB64: string): Uint8Array {
  if (!dataB64) return new Uint8Array(0);
  const binary = atob(dataB64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

/**
 * Encode a UTF-8 string as base64 so it round-trips through the WriteTerminal
 * binding. Using the byte-wise conversion keeps us compatible with keystrokes
 * that produce multi-byte UTF-8 characters.
 */
export function encodeTerminalInput(data: string): string {
  const encoded = new TextEncoder().encode(data);
  let binary = '';
  for (let i = 0; i < encoded.length; i++) {
    binary += String.fromCharCode(encoded[i]);
  }
  return btoa(binary);
}
