const diagramSources = new WeakMap<HTMLElement, string>();

export function rememberDiagramSource(element: HTMLElement, source: string): void {
  diagramSources.set(element, source);
}

export function readDiagramSource(element: HTMLElement): string | null {
  return diagramSources.get(element) ?? null;
}
