/**
 * Captures Chromium's deferred-notification ResizeObserver error without
 * letting the browser runner abort before the test can report every delivery.
 * Registering any window error listener changes Vitest's unhandled-error path,
 * so the ledger records every other ErrorEvent too. Use only inside a browser
 * test and always assert that `messages` is empty.
 */
export function captureResizeObserverLoopErrors(): {
  messages: string[];
  stop(): void;
} {
  const messages: string[] = [];
  const onError = (event: ErrorEvent): void => {
    const message = event.message ||
      (event.error instanceof Error ? event.error.message : String(event.error));
    messages.push(message);
    if (event.message.includes('ResizeObserver loop')) event.preventDefault();
  };
  window.addEventListener('error', onError);
  return {
    messages,
    stop: () => window.removeEventListener('error', onError),
  };
}
