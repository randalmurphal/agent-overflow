<script lang="ts">
  // Code-block host. Delegates the actual rendering (shiki highlight)
  // to svelte-streamdown's built-in Code component, but wraps it in a
  // `relative group` div that:
  //   1. Stamps the raw source on `data-code-source` so the markdown-
  //      aware copy serializer (`utils/markdownSerialize.ts`) can
  //      recover the original text after Shiki has wrapped it in
  //      token spans.
  //   2. Hosts a hover-revealed CopyButton overlay in the top-right
  //      corner. Streamdown's built-in chrome (header bar with
  //      language label + download/copy buttons) is suppressed via
  //      `controls={{ code: false }}` on the parent <Streamdown> and
  //      `theme.code.header: 'hidden'` so the only visible chrome is
  //      this single hover-revealed button — much sleeker for chat.
  //
  // The `group` Tailwind utility on the wrapper plus
  // `group-hover:opacity-100` on the button gives the same hover-
  // reveal pattern the legacy `.code-copy-mount` rule had in
  // `app.css`, but scoped to our new DOM shape.

  import Code from 'svelte-streamdown/code';
  import type { Tokens } from 'marked';
  import CopyButton from '../../primitives/CopyButton.svelte';
  import { addToast } from '../../../stores/toast.svelte';

  let { token, id }: { token: Tokens.Code; id: string } = $props();
</script>

<div
  class="streamdown-code-host group relative"
  data-code-source={token.text}
  data-code-lang={token.lang ?? ''}
>
  <Code {token} {id} />

  <div
    class="absolute top-1 right-1 z-10 opacity-0 transition-opacity duration-150 ease-out group-hover:opacity-100 focus-within:opacity-100"
  >
    <CopyButton
      text={token.text}
      label="Copy code"
      onError={() => addToast('error', 'Failed to copy')}
    />
  </div>
</div>
