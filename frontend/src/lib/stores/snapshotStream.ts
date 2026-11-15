// Convergence of a keyed frame stream with the answers of calls.
//
// The backend streams frames per key and answers calls with state, and the
// transport writes every event queued for a connection ahead of that
// connection's reply. So a frame of work a call started, or of anything that
// happened while it ran, can reach the client before the call's answer: an
// answer can predate frames already delivered, and it can name a key (a run,
// a terminal) the client could not route a frame to before it arrived. This
// primitive orders the two:
//
//   - While a call is in flight, the frames of the keys it holds are buffered,
//     and replayed in arrival order once its answer has been applied. A call
//     holds one key, or every key of the stream when its answer names a key
//     the client does not know yet.
//   - `hydrate` reads a key's whole state. Of overlapping reads of one key
//     only the newest decides; an older answer is discarded and leaves the
//     buffer to the newest. A failed read applies nothing and still replays.
//   - `claim` runs any call whose answer the store applies itself. Every
//     claim's answer is applied; a failed one rejects after the replay.
//   - A frame is buffered while any call holds its key, and frames of one key
//     are always folded in arrival order.
//   - A replayed frame cannot start another read of its key, so a stream that
//     outruns its snapshots does not read without end.
//
// The owning store keeps the state: what an answer means, and how a frame
// folds in. Its fold drops a frame the answer already carried, by the
// frame's sequence, and calls `hydrate` for a frame that shows frames were
// lost. A fold that applies every frame unconditionally makes a frame that
// arrived during the call win over its answer, which is correct when the
// producer publishes whole-state frames in the order it changes the state.

export interface SnapshotStreamOptions<F, S> {
  /** Prefix of the failed-read warning. */
  name: string;
  /** Reads the key's whole state. Required for `hydrate`. */
  read?(key: string): Promise<S>;
  /** Replaces the key's state with a snapshot. Required for `hydrate`. */
  applySnapshot?(key: string, snapshot: S): void;
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
  /**
   * Runs `call` holding `key`, or every key when `key` is null, and applies
   * its answer with `apply` before replaying the frames buffered meanwhile.
   * Resolves with the answer; rejects with the call's failure after the
   * replay.
   */
  claim<R>(key: string | null, call: () => Promise<R>, apply: (answer: R) => void): Promise<R>;
  /** A frame from the stream: buffered while a call holds its key, folded otherwise. */
  push(key: string, frame: F): void;
  /** Abandons every call in flight with its buffer. Later frames fold live. */
  reset(): void;
}

/**
 * Orders whole-state values a producer numbers itself (a per-process Seq),
 * whether they arrive as frames or as call answers: per key, the highest
 * number of the producer's current launch wins. The fold of a stream whose
 * producer publishes outside the lock that numbers its states uses this
 * instead of arrival order.
 */
export interface SequenceGate {
  /**
   * Whether a value numbered `seq` by launch `launch` is newer than what
   * `key` holds, recording it when it is. A value of another launch is
   * newer, since a restarted producer numbers from one again. An
   * unnumbered value (an older producer, or a state built locally) is
   * admitted and records nothing.
   */
  admit(key: string, seq: number | undefined, launch: string): boolean;
  /** Forgets every key. */
  reset(): void;
}

export function createSequenceGate(): SequenceGate {
  const held = new Map<string, { launch: string; seq: number }>();
  return {
    admit(key, seq, launch) {
      if (typeof seq !== 'number' || seq <= 0) return true;
      const current = held.get(key);
      if (current && current.launch === launch && seq <= current.seq) return false;
      held.set(key, { launch, seq });
      return true;
    },
    reset() {
      held.clear();
    },
  };
}

const FAILED = Symbol('failed');

export function createSnapshotStream<F, S = never>(options: SnapshotStreamOptions<F, S>): SnapshotStream<F> {
  // The newest read of each key. Its identity is the token: a read that is
  // no longer the key's entry was superseded or reset away.
  const reads = new Map<string, object>();
  // Claims in flight, of one key and of every key.
  const claims = new Map<string, number>();
  let claimsOfEveryKey = 0;
  // Frames of held keys, in arrival order.
  let held: Array<{ key: string; frame: F }> = [];
  // Bumped by reset, so a claim begun before it does not release one after.
  let generation = 0;
  // Replay is synchronous, so at most one key is replaying.
  let replaying: string | null = null;

  function holds(key: string): boolean {
    return claimsOfEveryKey > 0 || reads.has(key) || claims.has(key);
  }

  // Folds every buffered frame whose key nothing holds any more. A fold can
  // start a read of another key, whose later frames then stay buffered.
  function release(): void {
    if (held.length === 0) return;
    const ready = held;
    held = [];
    for (const entry of ready) {
      if (holds(entry.key)) {
        held.push(entry);
        continue;
      }
      replaying = entry.key;
      try {
        options.applyFrame(entry.key, entry.frame);
      } finally {
        replaying = null;
      }
    }
  }

  async function hydrate(key: string): Promise<void> {
    if (replaying === key) return;
    const { read, applySnapshot } = options;
    if (!read || !applySnapshot) throw new Error(`${options.name}: hydrate needs read and applySnapshot`);
    // Buffer from here, not from when the answer lands: a frame that arrives
    // mid-flight describes state the snapshot may predate.
    const token = {};
    reads.set(key, token);

    let snapshot: S | typeof FAILED;
    try {
      snapshot = await read(key);
    } catch (err) {
      snapshot = FAILED;
      console.warn(`${options.name}: hydrate ${key}: ${String(err)}`);
    }
    if (reads.get(key) !== token) return;
    reads.delete(key);
    if (snapshot !== FAILED) applySnapshot(key, snapshot);
    release();
  }

  async function claim<R>(key: string | null, call: () => Promise<R>, apply: (answer: R) => void): Promise<R> {
    const at = generation;
    if (key === null) claimsOfEveryKey += 1;
    else claims.set(key, (claims.get(key) ?? 0) + 1);
    const settle = (): void => {
      if (at !== generation) return;
      if (key === null) {
        claimsOfEveryKey -= 1;
        return;
      }
      const remaining = (claims.get(key) ?? 0) - 1;
      if (remaining > 0) claims.set(key, remaining);
      else claims.delete(key);
    };
    let answer: R;
    try {
      answer = await call();
    } catch (err) {
      settle();
      release();
      throw err;
    }
    try {
      apply(answer);
    } finally {
      settle();
      release();
    }
    return answer;
  }

  return {
    hydrate,
    claim,

    push(key: string, frame: F): void {
      if (holds(key)) {
        held.push({ key, frame });
        return;
      }
      options.applyFrame(key, frame);
    },

    reset(): void {
      generation += 1;
      reads.clear();
      claims.clear();
      claimsOfEveryKey = 0;
      held = [];
    },
  };
}
