// Convergence of a keyed frame stream with a full-snapshot read.
//
// The backend streams sequenced frames per key and answers a snapshot RPC
// with the key's whole state. A snapshot can predate frames that were
// delivered while it was being read, so this primitive orders the two:
//
//   - A frame that arrives while a read of its key is in flight is buffered,
//     and the buffer is replayed over the snapshot once it lands.
//   - Of overlapping reads of one key only the newest decides. An older
//     answer is discarded and leaves the buffer to the newest.
//   - A failed read applies no snapshot and still replays the buffer, so the
//     key does not stay buffering.
//   - A replayed frame cannot start another read of its key, so a stream that
//     outruns its snapshots does not read without end.
//
// The owning store keeps the state: what a snapshot means, and how a frame
// folds in. Its fold drops a frame the snapshot already carried, by the
// frame's sequence, and calls `hydrate` for a frame that shows frames were
// lost.

export interface SnapshotStreamOptions<F, S> {
  /** Prefix of the failed-read warning. */
  name: string;
  /** Reads the key's whole state. */
  read(key: string): Promise<S>;
  /** Replaces the key's state with a snapshot. */
  applySnapshot(key: string, snapshot: S): void;
  /** Folds one frame into the key's state. */
  applyFrame(key: string, frame: F): void;
}

export interface SnapshotStream<F> {
  /**
   * Reads the key's snapshot and reconciles. Settles after the buffered
   * frames have been replayed, and never rejects for a failed read. Does
   * nothing when called from the fold of a replayed frame of the same key.
   */
  hydrate(key: string): Promise<void>;
  /** A frame from the stream: buffered while a read of its key is in flight, folded otherwise. */
  push(key: string, frame: F): void;
  /** Abandons every read in flight with its buffer. Later frames fold live. */
  reset(): void;
}

const FAILED = Symbol('failed');

// The newest read of a key. Its identity is the token: a read that is no
// longer the key's entry was superseded or reset away. `frames` is shared
// with the read it superseded, so the newest read replays everything
// buffered since the oldest one began.
interface PendingRead<F> {
  readonly frames: F[];
}

export function createSnapshotStream<F, S>(options: SnapshotStreamOptions<F, S>): SnapshotStream<F> {
  const pending = new Map<string, PendingRead<F>>();
  // Replay is synchronous, so at most one key is replaying.
  let replaying: string | null = null;

  async function hydrate(key: string): Promise<void> {
    if (replaying === key) return;
    // Buffer from here, not from when the answer lands: a frame that arrives
    // mid-flight describes state the snapshot may predate.
    const read: PendingRead<F> = { frames: pending.get(key)?.frames ?? [] };
    pending.set(key, read);

    let snapshot: S | typeof FAILED;
    try {
      snapshot = await options.read(key);
    } catch (err) {
      snapshot = FAILED;
      console.warn(`${options.name}: hydrate ${key}: ${String(err)}`);
    }
    if (pending.get(key) !== read) return;
    pending.delete(key);

    if (snapshot !== FAILED) options.applySnapshot(key, snapshot);
    replaying = key;
    try {
      for (const frame of read.frames) options.applyFrame(key, frame);
    } finally {
      replaying = null;
    }
  }

  return {
    hydrate,

    push(key: string, frame: F): void {
      const read = pending.get(key);
      if (read) {
        read.frames.push(frame);
        return;
      }
      options.applyFrame(key, frame);
    },

    reset(): void {
      pending.clear();
    },
  };
}
