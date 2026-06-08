// Shared recorder for StubTerminalBody.svelte. The stub records each focus()
// call (by terminalID, in order) so a test can assert that focus lands on the
// newly-active terminal body after the keyed remount — the outcome the
// TerminalSurface focus latch exists to produce. Both the stub and the test
// import this module, so they share the same array instance.

export const terminalFocusCalls: string[] = [];

export function recordTerminalFocus(terminalID: string): void {
  terminalFocusCalls.push(terminalID);
}

export function resetTerminalFocusCalls(): void {
  terminalFocusCalls.length = 0;
}
