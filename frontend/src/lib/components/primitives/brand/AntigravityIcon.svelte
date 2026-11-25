<!--
  Google Antigravity official 4-color gradient brand mark. Rendered as a
  bundled static PNG asset so the exact multi-color Google gradient displays
  faithfully without monochrome tinting or inline SVG document overhead.
-->
<script lang="ts">
  import antigravityIconUrl from './antigravity.png';

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

<img
  src={antigravityIconUrl}
  alt=""
  aria-hidden="true"
  width={size}
  height={size}
  draggable="false"
  class="lucide-antigravity inline-block shrink-0 select-none object-contain {className}"
  style="width:{size}px;height:{size}px"
/>
