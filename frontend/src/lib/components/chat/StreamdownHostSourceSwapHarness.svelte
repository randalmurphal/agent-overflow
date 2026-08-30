<script lang="ts">
  // Test-only harness for exercising host prop reuse directly. Rendering
  // StreamdownMathHost / StreamdownMermaidHost outside a Streamdown tree fails
  // because their inner svelte-streamdown components require context; this
  // supplies the minimum context while keeping a single host instance mounted
  // across source changes.

  import { setContext } from 'svelte';
  import { mergeTheme } from 'svelte-streamdown';
  import type { Tokens } from 'marked';
  import StreamdownMathHost from './markdown/StreamdownMathHost.svelte';
  import StreamdownMermaidHost from './markdown/StreamdownMermaidHost.svelte';
  import { chatMarkdownTheme } from './markdown/streamdownTheme';

  type HostKind = 'math' | 'mermaid';
  type MathToken = {
    type: 'math';
    raw: string;
    text: string;
    isInline: boolean;
    displayMode: boolean;
  };

  let { kind, source }: { kind: HostKind; source: string } = $props();
  const fullTheme = mergeTheme(chatMarkdownTheme);

  setContext('streamdown', {
    pendingAsyncCount: 0,
    theme: fullTheme,
    mermaidConfig: { theme: 'default' },
    katexConfig: undefined,
    icons: undefined,
    registerAsyncResource() {
      return () => {};
    },
  });

  const mathToken: MathToken = $derived({
    type: 'math',
    raw: `$$${source}$$`,
    text: source,
    isInline: false,
    displayMode: true,
  });

  const mermaidToken: Tokens.Code = $derived({
    type: 'code',
    raw: '```mermaid\n' + source + '\n```',
    text: source,
    lang: 'mermaid',
  });
</script>

<div>
  {#if kind === 'math'}
    <StreamdownMathHost token={mathToken} id="source-swap-math" />
  {:else}
    <StreamdownMermaidHost token={mermaidToken} id="source-swap-mermaid" />
  {/if}
</div>
