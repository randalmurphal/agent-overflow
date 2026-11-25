<script lang="ts">
  // A tracker's image or attachment in ticket text (utils/trackerMedia.ts):
  // the file's name as a chip of plain text, or "image" when the text names
  // none. Nothing is fetched and nothing here is a control.
  import type { Tokens } from '../../../markdown';

  let { token }: { token: Pick<Tokens.Image, 'text'> } = $props();
</script>

<span data-tracker-media class="inline-block rounded bg-surface-2 px-1.5 text-sm text-fg-muted">{token.text || 'image'}</span>
