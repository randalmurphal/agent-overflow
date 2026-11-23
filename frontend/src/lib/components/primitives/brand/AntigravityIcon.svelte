<!--
  Google Antigravity brand mark (the delta/chevron prism). Rendered as a
  sprite-masked <span> so color follows currentColor like ClaudeIcon and
  OpenAIIcon.
-->
<script lang="ts" module>
  import { maskSpriteRef } from '../../../utils/maskSprite';

  const PATH =
    'M12 2L2.5 20.5c-.4.8.2 1.7 1.1 1.5l5.4-1.4c.5-.1.9-.5 1.1-1L12 14.5l1.9 5.1c.2.5.6.9 1.1 1l5.4 1.4c.9.2 1.5-.7 1.1-1.5L12 2zm0 5.6 2.8 7.4H9.2L12 7.6z';

  const maskRef = () => maskSpriteRef('brand:antigravity', 24, 24, 'fill="black"', `<path d="${PATH}"/>`);
</script>

<script lang="ts">
  interface Props {
    size?: number;
    class?: string;
    strokeWidth?: number;
  }

  let {
    size = 16,
    class: className = '',
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    strokeWidth: _strokeWidth = 2,
  }: Props = $props();
</script>

<span
  aria-hidden="true"
  class="mask-icon lucide-antigravity shrink-0 {className}"
  style="width:{size}px;height:{size}px;-webkit-mask-image:{maskRef()};mask-image:{maskRef()}"
></span>
