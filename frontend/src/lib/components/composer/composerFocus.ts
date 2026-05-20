export function composerRootFor(node: Element | undefined): HTMLElement | null {
  return node?.closest<HTMLElement>('[data-testid="composer-root"]') ?? null;
}

export function composerTextareaIn(root: HTMLElement | null): HTMLTextAreaElement | null {
  return root?.querySelector<HTMLTextAreaElement>('textarea[aria-label="Message Input"]') ?? null;
}

export function composerTextareaHasFocus(root: HTMLElement | null): boolean {
  const textarea = composerTextareaIn(root);
  return textarea !== null && document.activeElement === textarea;
}

