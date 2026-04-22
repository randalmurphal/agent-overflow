<script lang="ts">
  /*
   * Thin wrapper around lucide-svelte icons. One place to set default
   * size/stroke/opacity so every icon in the UI reads at the same
   * weight and is legible against the ambient noise overlay.
   *
   * Callers pass the imported lucide component via the `icon` prop:
   *
   *   import Search from 'lucide-svelte/icons/search';
   *   <Icon icon={Search} size={14} />
   *
   * Size is rendered as pixels via the SVG width/height attributes
   * rather than Tailwind utility classes because lucide itself drives
   * sizing through the attrs; mixing the two leads to sizing fights.
   */
  // `icon` accepts any Svelte component. lucide-svelte 1.0.1 ships as
  // Svelte 3/4 components (not the new `Component` type), so we type
  // this loosely. Svelte itself handles the instantiation on render.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type IconComponent = any;

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
