export interface ProjectNewThreadOptions {
  openInNewPane?: boolean;
}

export type ProjectNewThreadHandler = (
  projectId: string,
  options?: ProjectNewThreadOptions,
) => void | Promise<void>;

export function shouldOpenProjectThreadInNewPane(event: MouseEvent): boolean {
  return event.ctrlKey || event.metaKey;
}
