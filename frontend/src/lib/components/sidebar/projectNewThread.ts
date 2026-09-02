import { isModClick } from '../../utils/modClick';

export interface ProjectNewThreadOptions {
  openInNewPane?: boolean;
}

export type ProjectNewThreadHandler = (
  projectId: string,
  options?: ProjectNewThreadOptions,
) => void | Promise<void>;

export function shouldOpenProjectThreadInNewPane(event: MouseEvent): boolean {
  return isModClick(event);
}

/**
 * Create a terminal thread for a project (per-project `+terminal`) or, when
 * `projectId` is omitted, a standalone "home" terminal (the global
 * `+terminal`). Unlike new chat threads there is no `openInNewPane` option:
 * terminals always open in a fresh pane, so the gesture carries no modifier
 * meaning.
 */
export type ProjectNewTerminalHandler = (projectId?: string) => void | Promise<void>;
