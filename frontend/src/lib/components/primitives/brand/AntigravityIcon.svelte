<!--
  Google Antigravity official brand mark silhouette. Rendered as a
  sprite-masked <span> so color follows currentColor like ClaudeIcon and
  OpenAIIcon.
-->
<script lang="ts" module>
  import { maskSpriteRef } from '../../../utils/maskSprite';

  const PATH =
    'M21.751 22.607c1.34 1.005 3.35.335 1.508-1.508C17.73 15.74 18.904 1 12.037 1 5.17 1 6.342 15.74.815 21.1c-2.01 2.009.167 2.511 1.507 1.506 5.192-3.517 4.857-9.714 9.715-9.714 4.857 0 4.522 6.197 9.714 9.715z';

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
