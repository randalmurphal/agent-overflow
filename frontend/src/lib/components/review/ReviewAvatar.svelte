<script lang="ts">
  import { authorDisplayName, authorInitials, authorTone } from '../../utils/reviewIdentity';

  // An author's avatar disc: initials over a tone picked by login, so one
  // person keeps one color everywhere on the review surfaces. Drawn
  // locally; the forge's avatar image is never fetched. Tones are the
  // theme's own semantic colors, so a theme recolors them.

  interface Props {
    login: string;
    name?: string;
    /** Diameter in CSS px. */
    size?: number;
    /** Replaces the initials drawn from the name (the reader's own "You"). */
    initials?: string;
  }

  let { login, name, size = 28, initials }: Props = $props();

  const TONES = [
    'bg-accent/20 text-accent',
    'bg-info/20 text-info',
    'bg-success/20 text-success',
    'bg-warning/20 text-warning',
    'bg-error/20 text-error',
    'bg-provider-codex/20 text-provider-codex',
    'bg-provider-claude/20 text-provider-claude',
  ];

  const author = $derived({ authorLogin: login, authorName: name });
  const label = $derived(name ? `${authorDisplayName(author)} (@${login})` : `@${login}`);
</script>

<span
  class="inline-flex shrink-0 select-none items-center justify-center rounded-full font-semibold leading-none tracking-wide {TONES[authorTone(login)]}"
  style:width="{size}px"
  style:height="{size}px"
  style:font-size="{Math.max(8, Math.round(size * 0.38))}px"
  title={label}
  data-testid="review-avatar"
>{initials ?? authorInitials(author)}</span>
