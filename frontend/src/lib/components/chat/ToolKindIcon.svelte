<script lang="ts" module>
  import { maskSpriteRef } from '../../utils/maskSprite';

  // Tiny icon dispatcher — maps a ToolKindIcon name to a same-document
  // sprite mask (see utils/maskSprite.ts for why NOT an inline <svg> root
  // and NOT a data-URI mask-image). Inline shapes to avoid bundling an
  // icon library for a dozen icons.
  //
  // Color is keyed by `kind` via the --ico-* token family, shared with
  // everything else that speaks in tool hues (`TOOL_KIND_COLOR_CLASS`);
  // the mask span picks it up through currentColor.
  const STROKED = 'fill="none" stroke="black" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"';

  const SHAPES: Record<string, string> = {
    terminal: '<polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/>',
    file: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/>',
    eye: '<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>',
    search: '<circle cx="11" cy="11" r="7"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>',
    globe: '<circle cx="12" cy="12" r="10"/><line x1="2" y1="12" x2="22" y2="12"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/>',
    robot: '<rect x="4" y="7" width="16" height="12" rx="2"/><circle cx="9" cy="13" r="1"/><circle cx="15" cy="13" r="1"/><line x1="12" y1="3" x2="12" y2="7"/>',
    'speech-bubble': '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
    checklist: '<path d="M9 11l3 3 8-8"/><path d="M20 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h11"/>',
    puzzle: '<path d="M19.5 12A2.5 2.5 0 0 0 17 9.5V7a2 2 0 0 0-2-2h-2.5A2.5 2.5 0 0 0 10 7.5 2.5 2.5 0 0 0 12.5 10H13v2H9.5A2.5 2.5 0 0 0 7 14.5 2.5 2.5 0 0 0 9.5 17H12v3a1 1 0 0 0 1 1h2a1 1 0 0 0 1-1v-2.5A2.5 2.5 0 0 0 18.5 15 2.5 2.5 0 0 0 16 12.5H15v-.5h2.5A2.5 2.5 0 0 0 19.5 12z"/>',
    clock: '<circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>',
    // lucide `brain` icon (https://lucide.dev/icons/brain).
    brain: '<path d="M12 5a3 3 0 1 0-5.997.125 4 4 0 0 0-2.526 5.77 4 4 0 0 0 .556 6.588A4 4 0 1 0 12 18Z"/><path d="M12 5a3 3 0 1 1 5.997.125 4 4 0 0 1 2.526 5.77 4 4 0 0 1-.556 6.588A4 4 0 1 1 12 18Z"/><path d="M15 13a4.5 4.5 0 0 1-3-4 4.5 4.5 0 0 1-3 4"/><path d="M17.599 6.5a3 3 0 0 0 .399-1.375"/><path d="M6.003 5.125A3 3 0 0 0 6.401 6.5"/><path d="M3.477 10.896a4 4 0 0 1 .585-.396"/><path d="M19.938 10.5a4 4 0 0 1 .585.396"/><path d="M6 18a4 4 0 0 1-1.967-.516"/><path d="M19.967 17.484A4 4 0 0 1 18 18"/>',
    // lucide `list-collapse` icon (https://lucide.dev/icons/list-collapse):
    // three list lines collapsing into two carets — the compaction idiom.
    compaction: '<path d="M10 5h11"/><path d="M10 12h11"/><path d="M10 19h11"/><path d="m3 10 3-3-3-3"/><path d="m3 20 3-3-3-3"/>',
    // Generic "tool" fallback: lucide wrench. Reads as a tool rather than the
    // circle/exclamation idiom that the prior info-icon shape collapsed into
    // at small sizes.
    generic: '<path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/>',
  };

  function maskRefFor(kind: string): string {
    const known = kind in SHAPES ? kind : 'generic';
    return maskSpriteRef(`tool:${known}`, 24, 24, STROKED, SHAPES[known]);
  }
</script>

<script lang="ts">
  import { TOOL_KIND_COLOR_CLASS, type ToolKindIcon } from './toolCardHeader';

  let { kind, ariaLabel }: { kind: ToolKindIcon; ariaLabel?: string } = $props();

  const titleText = $derived(ariaLabel ?? `${kind} tool`);
  const colorClass = $derived(TOOL_KIND_COLOR_CLASS[kind]);
</script>

<span
  class="mask-icon h-3.5 w-3.5 shrink-0 {colorClass}"
  style="--mask-icon:{maskRefFor(kind)}"
  role="img"
  aria-label={titleText}
  data-icon={kind}
></span>
