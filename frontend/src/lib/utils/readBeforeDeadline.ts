export class ReadDeadlineError extends Error {
  constructor() { super('Computer did not answer in time.'); this.name = 'ReadDeadlineError'; }
}

// Bound optional startup reads. The original promise remains observed; a late
// answer is discarded unless its owner supplies a generation-checked callback.
// Never wrap a mutation: its outcome must remain visible.
export function readBeforeDeadline<T>(read: PromiseLike<T>, milliseconds: number, onLate?: (value: T) => void): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    let expired = false;
    const timer = setTimeout(() => { expired = true; reject(new ReadDeadlineError()); }, milliseconds);
    Promise.resolve(read).then((value) => {
      clearTimeout(timer);
      if (expired) {
        try { onLate?.(value); } catch (error) { console.error('Failed to apply a late computer response:', error); }
      }
      else resolve(value);
    }, (error) => { clearTimeout(timer); reject(error); });
  });
}
