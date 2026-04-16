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

/**
 * Decode a base64 output chunk into a plain JS string. Uses atob first and
 * then re-encodes through TextDecoder so control bytes and multi-byte
 * sequences survive intact.
 */
export function decodeTerminalOutput(dataB64: string): string {
  if (!dataB64) return '';
  const binary = atob(dataB64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  // Let xterm handle non-UTF-8 gracefully; we decode to Latin-1 so bytes
  // round-trip byte-for-byte to xterm.write.
  let out = '';
  for (let i = 0; i < bytes.length; i++) {
    out += String.fromCharCode(bytes[i]);
  }
  return out;
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
