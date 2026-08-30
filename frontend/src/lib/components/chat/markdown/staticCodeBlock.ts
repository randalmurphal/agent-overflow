import type { Tokens } from 'marked';
import { addToast } from '../../../stores/toast.svelte';
import { copyToClipboard } from '../../../utils/clipboard';
import { maskSpriteRef } from '../../../utils/maskSprite';
import { spanSegments, type EncodedLine } from '../../../utils/syntaxSpans';
import { getCachedBlockSpans } from './codeSpanCache';

type StreamdownContext = ReturnType<
  (typeof import('../../../markdown'))['useStreamdown']
>;

type CompletedCodeBlockRenderer = {
  render: (id: string) => string;
};

type CompletedRendererIdentity = {
  streamdown: StreamdownContext;
  lang: string;
  source: string;
};

// A completed code component may hold an authoritative local result which is
// deliberately absent from the cross-mount cache (a transient partial result
// or a failed highlight rendered plain). CompactBlocks owns the one DOM
// replacement, so the component publishes a synchronous renderer instead of
// replacing itself first. Entries live only as long as their mounted owner.
const completedCodeBlockRenderers = new Map<
  StreamdownContext,
  Map<string, Map<string, Map<object, CompletedCodeBlockRenderer>>>
>();
let completedRendererIdentities = new WeakMap<object, CompletedRendererIdentity>();

export function publishCompletedCodeBlockRenderer(
  owner: object,
  streamdown: StreamdownContext,
  lang: string,
  source: string,
  render: (id: string) => string,
): void {
  clearCompletedCodeBlockRenderer(owner);
  const byLang = completedCodeBlockRenderers.get(streamdown) ?? new Map();
  completedCodeBlockRenderers.set(streamdown, byLang);
  const bySource = byLang.get(lang) ?? new Map();
  byLang.set(lang, bySource);
  const byOwner = bySource.get(source) ?? new Map();
  bySource.set(source, byOwner);
  byOwner.set(owner, { render });
  completedRendererIdentities.set(owner, { streamdown, lang, source });
}

export function clearCompletedCodeBlockRenderer(owner: object): void {
  const identity = completedRendererIdentities.get(owner);
  if (!identity) return;
  completedRendererIdentities.delete(owner);
  const byLang = completedCodeBlockRenderers.get(identity.streamdown);
  const bySource = byLang?.get(identity.lang);
  const byOwner = bySource?.get(identity.source);
  byOwner?.delete(owner);
  if (byOwner?.size === 0) bySource?.delete(identity.source);
  if (bySource?.size === 0) byLang?.delete(identity.lang);
  if (byLang?.size === 0) completedCodeBlockRenderers.delete(identity.streamdown);
}

export function resetCompletedCodeBlockRenderersForTest(): void {
  completedCodeBlockRenderers.clear();
  completedRendererIdentities = new WeakMap();
}

export function codeFenceInfoWord(lang: string): string {
  for (let index = 0; index < lang.length; index += 1) {
    const ch = lang[index];
    if (ch === ' ' || ch === '\t') return lang.slice(0, index);
  }
  return lang;
}

const COPY_BUTTON_CLASS = [
  'inline-flex items-center justify-center rounded-md text-text-secondary',
  'transition-colors cursor-pointer hover:text-text-primary',
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
  'disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent',
  'disabled:hover:text-text-secondary h-7 w-7 bg-transparent hover:bg-surface-2/60',
].join(' ');

const CODE_COPY_OVERLAY_CLASS = [
  'absolute top-1 right-1 z-10 opacity-0 transition-opacity duration-150 ease-out',
  'group-hover/codeblock:opacity-100 focus-within:opacity-100',
].join(' ');

function escapeHtml(value: unknown): string {
  return String(value ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

function attribute(name: string, value: unknown): string {
  return ` ${name}="${escapeHtml(value)}"`;
}

function copyIconRef(): string {
  return maskSpriteRef(
    'static-code-copy',
    24,
    24,
    'fill="none" stroke="black" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"',
    '<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/>' +
      '<path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>',
  );
}

function checkIconRef(): string {
  return maskSpriteRef(
    'static-code-check',
    24,
    24,
    'fill="none" stroke="black" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"',
    '<path d="M20 6 9 17l-5-5"/>',
  );
}

function copyIconHtml(): string {
  return '<span aria-hidden="true" data-static-code-copy-icon' +
    attribute('style', `width:13px;height:13px;--mask-icon:${copyIconRef()}`) +
    ' class="lucide-icon lucide lucide-copy inline-block shrink-0 opacity-80"></span>';
}

/**
 * Serializes the settled code host from the same materialized lines and span
 * view as the live component. The volatile host keeps Svelte's per-line tree
 * because its final line changes. A completed block changes at most once more
 * when its exact highlight result arrives, so retaining thousands of keyed
 * each-block reactions behind immutable lines only wastes renderer memory.
 */
export function renderStaticCodeBlockHtml(
  token: Tokens.Code,
  id: string,
  streamdown: StreamdownContext,
  lines: readonly string[],
  lineSpans: (index: number) => EncodedLine | null,
): string {
  const code: string[] = [];
  for (let lineIndex = 0; lineIndex < lines.length; lineIndex++) {
    if (lineIndex > 0) code.push('\n');
    for (const segment of spanSegments(lines[lineIndex], lineSpans(lineIndex))) {
      if (segment.className) {
        code.push(
          '<span' + attribute('class', segment.className) + '>' +
            escapeHtml(segment.text) +
          '</span>',
        );
      } else {
        code.push(escapeHtml(segment.text));
      }
    }
  }

  return '<div class="streamdown-code-host group/codeblock relative" data-code-source=""' +
    attribute('data-code-lang', token.lang ?? '') +
    '><div' +
      attribute('data-streamdown-code', id) +
      attribute('class', streamdown.theme.code.base) +
    '><div style="height: fit-content; width: 100%;"' +
      attribute('class', streamdown.theme.code.container) +
    '><pre' + attribute('class', streamdown.theme.code.pre) + '><code>' +
      code.join('') +
    '</code></pre></div></div>' +
    '<div' + attribute('class', CODE_COPY_OVERLAY_CLASS) + '>' +
      '<button type="button" aria-label="Copy code" title="Copy code"' +
        attribute('class', COPY_BUTTON_CLASS) +
        ' data-icon-button data-static-code-copy>' +
        copyIconHtml() +
      '</button>' +
    '</div></div>';
}

/**
 * Lets the completed-block owner bypass a code component when its exact
 * highlight result is already cached. A cache miss returns null so the real
 * host mounts, performs the request, and keeps Streamdown's async-settle gate
 * authoritative. The host publishes a per-record retry when its own result is
 * ready; an unrelated live code request must not hold every completed island.
 */
export function renderCachedStaticCodeBlockHtml(
  token: Tokens.Code,
  id: string,
  streamdown: StreamdownContext,
): string | null {
  const lang = codeFenceInfoWord(token.lang ?? '');
  const completed = completedCodeBlockRenderers
    .get(streamdown)
    ?.get(lang)
    ?.get(token.text)
    ?.values()
    .next().value;
  if (completed) {
    return completed.render(id);
  }
  const spans = lang ? getCachedBlockSpans(lang, token.text) : null;
  if (lang && spans === null) return null;
  const lines = token.text.split('\n');
  return renderStaticCodeBlockHtml(
    token,
    id,
    streamdown,
    lines,
    (index) => spans?.[index] ?? null,
  );
}

let copyDelegateInstalled = false;
const copyResetTimers = new WeakMap<HTMLButtonElement, ReturnType<typeof setTimeout>>();

function reportStaticCodeCopyFailure(error: unknown): void {
  console.error('[static-code-copy] handler failed', error);
  addToast('error', 'Failed to copy');
}

function codeCopyButtonFor(target: EventTarget | null): HTMLButtonElement | null {
  const element = target instanceof Element
    ? target
    : target instanceof Node
      ? target.parentElement
      : null;
  return element?.closest<HTMLButtonElement>('button[data-static-code-copy]') ?? null;
}

function setCopiedState(button: HTMLButtonElement, copied: boolean): void {
  const icon = button.querySelector<HTMLElement>('[data-static-code-copy-icon]');
  if (!icon) {
    throw new Error('static code copy button lost its icon');
  }
  const label = copied ? 'Copied' : 'Copy code';
  button.setAttribute('aria-label', label);
  button.title = label;
  icon.style.setProperty('--mask-icon', copied ? checkIconRef() : copyIconRef());
  icon.classList.toggle('lucide-copy', !copied);
  icon.classList.toggle('lucide-check', copied);
}

async function handleStaticCodeCopy(event: MouseEvent): Promise<void> {
  const button = codeCopyButtonFor(event.target);
  if (!button) return;
  const host = button.closest<HTMLElement>('.streamdown-code-host');
  const code = host?.querySelector('pre > code');
  if (!host || !code) {
    console.error('[static-code-copy] code host is incomplete');
    addToast('error', 'Failed to copy');
    return;
  }

  const text = code.textContent ?? '';
  // Match CopyButton: an empty code block has nothing to copy and does not
  // enter the transient "Copied" state.
  if (!text) return;

  const ok = await copyToClipboard(text);
  if (!ok) {
    addToast('error', 'Failed to copy');
    return;
  }
  setCopiedState(button, true);
  const previousTimer = copyResetTimers.get(button);
  if (previousTimer) clearTimeout(previousTimer);
  const timer = setTimeout(() => {
    copyResetTimers.delete(button);
    if (!button.isConnected) return;
    try {
      setCopiedState(button, false);
    } catch (error) {
      reportStaticCodeCopyFailure(error);
    }
  }, 2000);
  copyResetTimers.set(button, timer);
}

/** One document listener replaces one Svelte CopyButton instance per fence. */
export function ensureStaticCodeCopyDelegate(): void {
  if (copyDelegateInstalled) return;
  document.addEventListener('click', (event) => {
    void handleStaticCodeCopy(event).catch(reportStaticCodeCopyFailure);
  });
  copyDelegateInstalled = true;
}
