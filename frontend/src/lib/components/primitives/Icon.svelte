<script lang="ts">
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';

  /*
   * Thin wrapper around @lucide/svelte icons. One place to set default
   * size/stroke/opacity so every icon in the UI reads at the same
   * weight and is legible against the ambient noise overlay.
   *
   * Callers pass the imported lucide component via the `icon` prop:
   *
   *   import Search from '@lucide/svelte/icons/search';
   *   <Icon icon={Search} size={14} />
   *
   * Size is rendered as pixels via the SVG width/height attributes
   * rather than Tailwind utility classes because lucide itself drives
   * sizing through the attrs; mixing the two leads to sizing fights.
   */
  type IconComponent = Component<LucideProps>;

  interface Props {
    icon: IconComponent;
    size?: number;
    strokeWidth?: number;
    class?: string;
  }

  let {
    icon: IconComponent,
    size = 16,
    strokeWidth = 2,
    class: className = '',
  }: Props = $props();
</script>

<IconComponent {size} {strokeWidth} class="inline-block shrink-0 opacity-80 {className}" />
