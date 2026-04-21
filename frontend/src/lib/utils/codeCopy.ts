/**
 * Single document-level delegated click handler for `.ch-copy` buttons
 * emitted by the Go-side markdown renderer. Each button sits next to a
 * sibling `<pre>` element; the click reads that element's textContent
 * into the clipboard. Keeping one global listener instead of wiring a
 * handler per button means streaming `{@html}` replacements don't need
 * re-binding.
 */

const DATA_STATE = 'copyState';
const RESET_MS = 1200;

let registered = false;

export function registerCodeCopyListener(): void {
  if (registered || typeof document === 'undefined') return;
  document.addEventListener('click', handleClick);
  registered = true;
}

async function handleClick(evt: MouseEvent): Promise<void> {
  const target = evt.target;
  if (!(target instanceof HTMLElement)) return;
  const btn = target.closest<HTMLButtonElement>('button.ch-copy');
  if (!btn) return;
  evt.preventDefault();

  const pre = btn.parentElement?.querySelector<HTMLElement>('pre');
  if (!pre) return;
  const code = pre.textContent ?? '';

  try {
    await navigator.clipboard.writeText(code);
    setCopiedState(btn, 'copied');
  } catch {
    setCopiedState(btn, 'error');
  }
}

function setCopiedState(btn: HTMLButtonElement, state: 'copied' | 'error'): void {
  btn.dataset[DATA_STATE] = state;
  const label = btn.textContent;
  btn.textContent = state === 'copied' ? 'Copied' : 'Failed';
  window.setTimeout(() => {
    btn.textContent = label;
    delete btn.dataset[DATA_STATE];
  }, RESET_MS);
}
