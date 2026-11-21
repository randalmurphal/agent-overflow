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
// and a retired run cannot show, collect or send anything: `beginSubmit`
// answers false, which is how the call it ends in is gated.
import type { ProjectDeletionPreview } from '../../types/workflow';
import type { ProjectTarget } from './sidebarEntries';

export type ProjectActionKind = 'archive' | 'delete' | 'rename';

export interface ProjectActionRun {
  readonly kind: ProjectActionKind;
  /** The project this run acts on, frozen at the gesture that started it. */
  readonly target: ProjectTarget;
  /** True once a newer run replaced it. */
  readonly retired: boolean;
  /** Whether this run is showing its confirmation. */
  readonly confirming: boolean;
  /** What deleting involves, once the computer that holds it has answered. */
  readonly preview: ProjectDeletionPreview | null;
  /** True from the moment the run took itself for its call. */
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
  /** Finish the run, shown or not. */
  end(): void;
}

export interface ProjectActions {
  /** The run the row is showing. The only run anything renders. */
  readonly current: ProjectActionRun | null;
  start(kind: ProjectActionKind, target: ProjectTarget): ProjectActionRun;
}

class Run implements ProjectActionRun {
  readonly kind: ProjectActionKind;
  readonly target: ProjectTarget;
  #retired = $state(false);
  #confirming = $state(false);
  #preview: ProjectDeletionPreview | null = $state.raw(null);
  #submitting = $state(false);
  #name = $state('');
  readonly #finish: (run: Run) => void;

  constructor(kind: ProjectActionKind, target: ProjectTarget, finish: (run: Run) => void) {
    this.kind = kind;
    this.target = target;
    this.#name = target.name;
    this.#finish = finish;
  }

  get retired(): boolean {
    return this.#retired;
  }

  get confirming(): boolean {
    return this.#confirming;
  }

  get preview(): ProjectDeletionPreview | null {
    return this.#preview;
  }

  get submitting(): boolean {
    return this.#submitting;
  }

  get name(): string {
    return this.#name;
  }

  confirm(): void {
    if (this.#retired) return;
    this.#confirming = true;
  }

  offer(preview: ProjectDeletionPreview): void {
    if (this.#retired) return;
    this.#preview = preview;
  }

  setName(name: string): void {
    if (this.#retired) return;
    this.#name = name;
  }

  beginSubmit(): boolean {
    if (this.#retired || this.#submitting) return false;
    this.#submitting = true;
    return true;
  }

  end(): void {
    this.#finish(this);
  }

  retire(): void {
    this.#retired = true;
  }
}

/** The runs of one project row. One at a time, newest only. */
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
