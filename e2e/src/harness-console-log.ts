import { createWriteStream, type WriteStream } from 'node:fs';
import { mkdir } from 'node:fs/promises';
import * as path from 'node:path';

/**
 * The backend's console, stdout and stderr lines in arrival order, kept in
 * a file beside the backend's own logs (`<root>/agent-overflow/logs`, the
 * directory `ao-harness up` also writes its console captures into). The
 * file lives and dies with the data root, so memory stays flat however
 * long a worker runs, and a kept root carries its console with it.
 * Opened in append mode: a spec that restarts a backend on the same root
 * gets every boot in one file.
 */
export class BackendConsoleLog {
  static readonly fileName = 'harness-console.log';

  private failure?: Error;
  private closing?: Promise<void>;
  private readonly streamClosed: Promise<void>;

  private constructor(
    readonly path: string,
    private readonly stream: WriteStream,
  ) {
    stream.on('error', (error) => {
      if (this.failure) return;
      this.failure = error;
      // Reported here, once. Teardown closes this file on paths that must
      // not fail over a diagnostic; flush() rethrows it to a copier.
      console.error(`harness console log ${this.path} failed: ${error.message}`);
    });
    this.streamClosed = new Promise((resolve) => stream.once('close', () => resolve()));
  }

  static async open(dataRoot: string): Promise<BackendConsoleLog> {
    const dir = path.join(dataRoot, 'agent-overflow', 'logs');
    await mkdir(dir, { recursive: true, mode: 0o700 });
    const file = path.join(dir, BackendConsoleLog.fileName);
    return new BackendConsoleLog(file, createWriteStream(file, { flags: 'a', mode: 0o600 }));
  }

  write(line: string): void {
    if (this.closing || this.failure) return;
    this.stream.write(`${line}\n`);
  }

  /** Resolves once every line written so far is in the file. */
  async flush(): Promise<void> {
    if (this.closing) {
      await this.closing;
    } else if (!this.failure) {
      // Write callbacks run in order, so an empty write's callback means
      // every earlier line has reached the file.
      await new Promise<void>((resolve) => this.stream.write('', () => resolve()));
    }
    if (this.failure) {
      throw new Error(`harness console log ${this.path} failed: ${this.failure.message}`);
    }
  }

  /**
   * Ends the file and resolves once its descriptor is closed. A data root
   * holding an open file cannot be removed on Windows, so every removal
   * of a root waits for this or `closed` first. Safe to call repeatedly.
   */
  async close(): Promise<void> {
    if (!this.closing) {
      this.closing = this.streamClosed;
      if (!this.stream.closed) this.stream.end();
    }
    await this.closing;
  }

  /** Resolves once the file is closed by close() or by a stream failure. */
  closed(): Promise<void> {
    return this.streamClosed;
  }
}
