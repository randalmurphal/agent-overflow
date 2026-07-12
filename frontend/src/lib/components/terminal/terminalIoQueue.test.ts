import { describe, expect, it, vi } from 'vitest';
import {
  createTerminalInputWriter,
  createTerminalResizeWriter,
} from './terminalIoQueue';

function deferred() {
  let resolve!: () => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<void>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('createTerminalInputWriter', () => {
  it('serializes writes and coalesces input that arrives behind an in-flight call', async () => {
    const first = deferred();
    const send = vi.fn()
      .mockReturnValueOnce(first.promise)
      .mockResolvedValue(undefined);
    const writer = createTerminalInputWriter(send, vi.fn());

    writer.write('a');
    writer.write('b');
    writer.write('c');

    expect(send).toHaveBeenCalledTimes(1);
    expect(send).toHaveBeenCalledWith('a');

    first.resolve();
    await first.promise;
    await writer.idle();

    expect(send).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenLastCalledWith('bc');
  });

  it('reports a failed batch and continues with later input', async () => {
    const first = deferred();
    const onError = vi.fn();
    const send = vi.fn()
      .mockReturnValueOnce(first.promise)
      .mockResolvedValue(undefined);
    const writer = createTerminalInputWriter(send, onError);

    writer.write('a');
    writer.write('b');
    first.reject(new Error('write failed'));
    await first.promise.catch(() => {});
    await Promise.resolve();

    expect(onError).toHaveBeenCalledWith(expect.objectContaining({ message: 'write failed' }));
    expect(send).toHaveBeenLastCalledWith('b');
  });

  it('reports idle only after the active write and queued input both finish', async () => {
    const first = deferred();
    const send = vi.fn()
      .mockReturnValueOnce(first.promise)
      .mockResolvedValue(undefined);
    const writer = createTerminalInputWriter(send, vi.fn());
    const becameIdle = vi.fn();

    writer.write('a');
    writer.write('b');
    void writer.idle().then(becameIdle);
    await Promise.resolve();
    expect(becameIdle).not.toHaveBeenCalled();

    first.resolve();
    await first.promise;
    await writer.idle();

    expect(send).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenLastCalledWith('b');
    expect(becameIdle).toHaveBeenCalledOnce();
  });
});

describe('createTerminalResizeWriter', () => {
  it('serializes resizes and sends only the newest queued geometry', async () => {
    const first = deferred();
    const send = vi.fn()
      .mockReturnValueOnce(first.promise)
      .mockResolvedValue(undefined);
    const writer = createTerminalResizeWriter(send, vi.fn());

    writer.resize(24, 80);
    writer.resize(30, 100);
    writer.resize(40, 120);

    expect(send).toHaveBeenCalledTimes(1);
    expect(send).toHaveBeenCalledWith(24, 80);

    first.resolve();
    await first.promise;
    await Promise.resolve();

    expect(send).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenLastCalledWith(40, 120);
  });

  it('does not resend a size that completed successfully', async () => {
    const send = vi.fn().mockResolvedValue(undefined);
    const writer = createTerminalResizeWriter(send, vi.fn());

    writer.resize(24, 80);
    await Promise.resolve();
    writer.resize(24, 80);

    expect(send).toHaveBeenCalledTimes(1);
  });

  it('does not resend the active size when newer queued measurements collapse back to it', async () => {
    const first = deferred();
    const send = vi.fn()
      .mockReturnValueOnce(first.promise)
      .mockResolvedValue(undefined);
    const writer = createTerminalResizeWriter(send, vi.fn());

    writer.resize(24, 80);
    writer.resize(30, 100);
    writer.resize(24, 80);
    first.resolve();
    await first.promise;
    await Promise.resolve();

    expect(send).toHaveBeenCalledTimes(1);
    expect(send).toHaveBeenCalledWith(24, 80);
  });
});
