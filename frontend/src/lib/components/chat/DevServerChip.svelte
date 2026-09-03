<script lang="ts">
  // "Open in browser" for a dev server a command announced. Rendered in a
  // command row's header actions, so it is reachable without expanding the
  // output — the point of the affordance is that you saw the agent start
  // `npm run dev` and want the browser NOW.
  //
  // The click is kept off the enclosing row: the chip sits next to a
  // disclosure toggle and inside surfaces that handle row clicks of their
  // own. Opening always routes through handleExternalURL — the one
  // external-open wrapper: it re-validates the URL, then either hands it
  // to the Go binding (which owns the WSL → Windows browser bridge) or
  // falls back to window.open in a remote client session.
  //
  // With a `preview`, the port is on another machine and `localhost` here
  // is not it. The click mints a URL on that machine's port gateway
  // instead (`stores/devServers#openPreview`), which is a different
  // destination and so a different sentence on the button.
  import { devServerLabel, handleExternalURL } from '../../utils/externalLinks';
  import { openPreview, type PreviewChip } from '../../stores/devServers.svelte';

  let { url, preview }: { url: string; preview?: PreviewChip } = $props();

  let label = $derived(devServerLabel(url));
  let action = $derived(
    preview ? `Open ${label} on ${preview.machine}` : `Open ${url} in browser`,
  );

  function open(event: MouseEvent): void {
    event.stopPropagation();
    if (preview) {
      void openPreview(preview.threadId, preview.port, preview.path);
      return;
    }
    void handleExternalURL(url);
  }
</script>

<button
  type="button"
  class="inline-flex shrink-0 items-center gap-1 rounded-full border border-accent/30 bg-accent/10 px-1.5 py-0.5
         text-[0.6875rem] font-medium text-accent cursor-pointer hover:bg-accent/20
         focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  onclick={open}
  title={action}
  aria-label={action}
  data-testid="dev-server-chip"
  data-url={url}
  data-machine={preview?.machine}
>
  <svg
    class="size-3"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path d="M15 3h6v6" />
    <path d="M10 14 21 3" />
    <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
  </svg>
  {label}
</button>
