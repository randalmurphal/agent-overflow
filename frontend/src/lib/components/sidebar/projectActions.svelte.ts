// One run of one action on one project row.
//
// Archive, Delete and Rename all start from a gesture, then wait: for a
// confirmation, for what deleting involves, for the user to finish typing.
// A row is keyed by its repository and outlives a change of which checkout
// represents it, so each of those waits can end after the row has changed
// hands. Holding the target in one field and what the run collected in
// another made that a per-field race: a preview could be installed beside a
// target from a later gesture and the call took one from each.
//
// A run owns its target and everything derived from it, and only the row's
// current run is shown or sent. Starting a run retires the one before it,
// and a run that is not live cannot show, collect or send anything:
// `beginSubmit` answers false, which is how the call it ends in is gated.
// A run leaves `live` when it is replaced, when it is ended and when it
// takes itself for its call, so nothing that is not the row's current run
// can still act, and nothing current can act twice.
import type { ProjectDeletionPreview } from '../../types/workflow';
import type { ProjectTarget } from './sidebarEntries';

export type ProjectActionKind = 'archive' | 'delete' | 'rename';

export interface ProjectActionRun {
  readonly kind: ProjectActionKind;
  /** The project this run acts on, frozen at the gesture that started it. */
  readonly target: ProjectTarget;
  /** True once the run is no longer live: replaced, ended or submitted. */
  readonly retired: boolean;
  /** Whether this run is showing its confirmation. */
  readonly confirming: boolean;
  /** What deleting involves, once the computer that holds it has answered. */
  readonly preview: ProjectDeletionPreview | null;
  /** True while the run's call is out. */
  readonly submitting: boolean;
  /** The name a rename is editing: the target's, as the user changes it. */
  readonly name: string;
  /** Show this run's confirmation. */
  confirm(): void;
  /** Install what the computer answered about deleting this run's target. */
  offer(preview: ProjectDeletionPreview): void;
  setName(name: string): void;
  /** Take the run for the call it ends in, or answer false because it was
   *  retired and nothing it collected may be sent. */
  beginSubmit(): boolean;
  /** Finish the run, shown or not. It is not live afterwards. */
  end(): void;
}

export interface ProjectActions {
  /** The run the row is showing. The only run anything renders. */
  readonly current: ProjectActionRun | null;
  start(kind: ProjectActionKind, target: ProjectTarget): ProjectActionRun;
}

// A run's whole life. It leaves `live` once, and never comes back: the user
// replaced it, finished with it, or it took itself for its call. Everything
// a run collects is refused outside `live`, `beginSubmit` included, so a
// callback captured before any of those cannot act afterwards.
type RunState = 'live' | 'submitted' | 'done';

class Run implements ProjectActionRun {
  readonly kind: ProjectActionKind;
  readonly target: ProjectTarget;
  #state: RunState = $state('live');
  #confirming = $state(false);
  #preview: ProjectDeletionPreview | null = $state.raw(null);
  #name = $state('');
  readonly #finish: (run: Run) => void;

  constructor(kind: ProjectActionKind, target: ProjectTarget, finish: (run: Run) => void) {
    this.kind = kind;
    this.target = target;
    this.#name = target.name;
    this.#finish = finish;
  }

  get retired(): boolean {
    return this.#state !== 'live';
  }

  get confirming(): boolean {
    return this.#confirming;
  }

  get preview(): ProjectDeletionPreview | null {
    return this.#preview;
  }

  get submitting(): boolean {
    return this.#state === 'submitted';
  }

  get name(): string {
    return this.#name;
  }

  confirm(): void {
    if (this.retired) return;
    this.#confirming = true;
  }

  offer(preview: ProjectDeletionPreview): void {
    if (this.retired) return;
    this.#preview = preview;
  }

  setName(name: string): void {
    if (this.retired) return;
    this.#name = name;
  }

  beginSubmit(): boolean {
    if (this.retired) return false;
    this.#state = 'submitted';
    return true;
  }

  end(): void {
    // Done first: a run stops being live when it is ended, not only when
    // something newer replaces it.
    this.#state = 'done';
    this.#finish(this);
  }

  retire(): void {
    this.#state = 'done';
  }
}

/**
 * The runs of one project row. One at a time, newest only: the run that is
 * current is the only live one, and a run that stops being current stops
 * being live in the same step.
 */
export function createProjectActions(): ProjectActions {
  let current: Run | null = $state.raw(null);
  return {
    get current(): ProjectActionRun | null {
      return current;
    },
    start(kind: ProjectActionKind, target: ProjectTarget): ProjectActionRun {
      current?.retire();
      current = new Run(kind, target, (run) => {
        if (current === run) current = null;
      });
      return current;
    },
  };
}
