// Shared recorder for StubTerminalBody.svelte. The stub records each focus()
// call (by terminalID, in order) so a test can assert that focus lands on the
// newly-active terminal body after the keyed remount — the outcome the
// TerminalSurface focus latch exists to produce. It also records each call
// its find provider receives, as `terminalID:call`. Both the stub and the
// test import this module, so they share the same array instances.

export const terminalFocusCalls: string[] = [];
export const terminalFindCalls: string[] = [];

export function recordTerminalFocus(terminalID: string): void {
  terminalFocusCalls.push(terminalID);
}

export function recordTerminalFind(terminalID: string, call: string): void {
  terminalFindCalls.push(`${terminalID}:${call}`);
}

export function resetTerminalFocusCalls(): void {
  terminalFocusCalls.length = 0;
  terminalFindCalls.length = 0;
}
