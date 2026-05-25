<script lang="ts">
  /*
   * Sub-line under errored / killed / declined tool-call rows. Renders
   * an optional short `code` chip (e.g. `exit 137`, `signal 9`) and a
   * one-line `msg`, both in the matching tone. Replaces the inline
   * "text-error" badges and ad-hoc exit-code pills the individual
   * renderers used before.
   *
   * Body-column alignment (clearing the chev + icon + label gutters)
   * is the caller's responsibility — RowError stays geometry-agnostic
   * so the same component can sit beneath any row regardless of the
   * gutter widths the row composes. Callers typically render this
   * inside the same body-column wrapper as the row's main content.
   */

  interface Props {
    /**
     * Visual tone. `error` uses the red palette (errored, killed,
     * non-zero exit); `declined` uses the amber palette (user
     * declined / dismissed an approval).
     */
    tone: 'error' | 'declined';
    /**
     * The human-readable error message. Required even when `code` is
     * present so screen readers and copy-paste get the full text.
     */
    msg: string;
    /**
     * Optional short scalar (e.g. `exit 137`, `signal 9`, `404`).
     * Rendered as a monospace chip preceding `msg`. Omit when there
     * is no machine-readable code to surface.
     */
    code?: string;
    class?: string;
  }

  let { tone, msg, code, class: className = '' }: Props = $props();

  const chipClass = $derived(
    tone === 'error'
      ? 'bg-error/10 text-error'
      : 'bg-warning/10 text-warning',
  );
  const msgClass = $derived(
    tone === 'error' ? 'text-error/90' : 'text-warning/90',
  );
</script>

<div
  class="flex items-baseline gap-1.5 text-[0.75rem] {className}"
  data-testid="row-error"
  data-tone={tone}
  role="status"
>
  {#if code}
    <span
      class="inline-flex shrink-0 items-center rounded px-1 py-px font-mono text-[0.625rem] {chipClass}"
      data-testid="row-error-code"
    >{code}</span>
  {/if}
  <span class="min-w-0 break-words {msgClass}" data-testid="row-error-msg">{msg}</span>
</div>
