import type { DraftMode } from '../../stores/threadCreation.svelte';

export interface ProjectNewThreadOptions {
  openInNewPane?: boolean;
  mode?: DraftMode;
}

export type ProjectNewThreadHandler = (
  projectId: string,
  options?: ProjectNewThreadOptions,
) => void | Promise<void>;

export function shouldOpenProjectThreadInNewPane(event: MouseEvent): boolean {
  return event.ctrlKey || event.metaKey;
}
