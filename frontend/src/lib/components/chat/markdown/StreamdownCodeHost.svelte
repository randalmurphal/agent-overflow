<script lang="ts">
  // Code-block host. Delegates the actual rendering (shiki highlight,
  // header chrome, copy/download buttons) to svelte-streamdown's
  // built-in Code component, but stashes the raw source on a wrapping
  // `<div data-code-source="...">` so the markdown-aware copy
  // serializer (`utils/markdownSerialize.ts`) can recover the original
  // text after Shiki has wrapped it in token spans.
  //
  // textContent of a Shiki-highlighted code block already round-trips
  // back to the source text (the spans are inert wrappers), so the
  // data attribute is a belt-and-braces guarantee against future Shiki
  // changes — and lets the serializer skip the chrome divs cleanly.

  import Code from 'svelte-streamdown/code';
  import type { Tokens } from 'marked';

  let { token, id }: { token: Tokens.Code; id: string } = $props();
</script>

<div class="streamdown-code-host" data-code-source={token.text} data-code-lang={token.lang ?? ''}>
  <Code {token} {id} />
</div>
