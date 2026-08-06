<script lang="ts">
  // Reusable copy-to-clipboard button. Wraps IconButton with a Copy↔Check
  // icon swap and an internal 2s reset timer. The `text` prop accepts a
  // string or a sync/async getter so that callers with lazy-loaded content
  // (e.g. proposed plans) can defer the fetch until click.
  //
  // Failure feedback: callers wire it via `onError`. The primitive itself
  // stays leaf-pure — no `stores/` import — so the entire primitives layer
  // can be extracted as a standalone package without modifications.
  // Success feedback is the icon swap only — no toast — to keep the UI
  // quiet on the common path.
  //
  // `write` is the same leaf-purity story: a markdown surface wants the
  // clipboard to carry a `text/html` flavor too, but the markdown
  // serializer pulls in the whole marked pipeline. Injecting the writer
  // keeps that dependency at the call site (`copyMarkdownToClipboard`)
  // instead of in the primitives layer, and keeps the flavor policy in
  // one module rather than one branch per button.
  import { onDestroy } from 'svelte';
  import Copy from '@lucide/svelte/icons/copy';
  import Check from '@lucide/svelte/icons/check';
  import Icon from './Icon.svelte';
  import IconButton from './IconButton.svelte';
  import { copyToClipboard } from '../../utils/clipboard';

  interface Props {
    text: string | (() => string | Promise<string>);
    /** Clipboard writer. Defaults to a plain-text write. */
    write?: (value: string) => Promise<boolean>;
    label?: string;
    copiedLabel?: string;
    size?: 'sm' | 'md';
    iconSize?: number;
    variant?: 'ghost' | 'subtle';
    onError?: () => void;
    disabled?: boolean;
  }

  let {
    text,
    write = copyToClipboard,
    label = 'Copy',
    copiedLabel = 'Copied',
    size = 'sm',
    iconSize = 13,
    variant = 'ghost',
    onError,
    disabled = false,
  }: Props = $props();

  let copied = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;

  onDestroy(() => {
    if (timer) clearTimeout(timer);
  });

  async function handleCopy(): Promise<void> {
    try {
      const value = typeof text === 'function' ? await text() : text;
      if (!value) return;
      const ok = await write(value);
      if (!ok) {
        onError?.();
        return;
      }
    } catch {
      onError?.();
      return;
    }
    copied = true;
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => {
      copied = false;
      timer = undefined;
    }, 2000);
  }
</script>

<IconButton
  label={copied ? copiedLabel : label}
  {size}
  {variant}
  {disabled}
  onClick={() => void handleCopy()}
>
  {#snippet children()}
    <Icon icon={copied ? Check : Copy} size={iconSize} />
  {/snippet}
</IconButton>
