<script lang="ts">
  // RemoteEndpointForm: the add/edit form. Owns the input fields,
  // client-side validation, and the reveal toggle for the token input.
  // The parent decides whether this is "Add" or "Save" via the mode
  // prop and supplies the existing values on edit.
  //
  // Why not own persistence here? Add and Update are list-mutating
  // RPCs whose result needs to update the surrounding list state.
  // Keeping the form pure (it produces a {name, url, token} payload)
  // lets the parent drive the binding call and stitch the response
  // into the list.

  interface Props {
    mode: 'add' | 'edit';
    initialName: string;
    initialURL: string;
    initialToken: string;
    initialError?: string | null;
    saving: boolean;
    onSubmit: (values: { name: string; url: string; token: string }) => void;
    onCancel: () => void;
  }

  const {
    mode,
    initialName,
    initialURL,
    initialToken,
    initialError = null,
    saving,
    onSubmit,
    onCancel,
  }: Props = $props();

  // Local working buffer for the inputs. We seed from the initial*
  // props on mount and re-sync when those props change — the parent
  // updates initialToken after the GetRemoteEndpointToken fetch
  // resolves, and we want the field to populate without a full
  // remount that would lose any tweaks the user made first.
  //
  // Reading props directly inside $state() captures the initial
  // value only — Svelte's `state_referenced_locally` would warn.
  // Initialising to '' and letting the $effect block do the
  // first-and-subsequent sync is functionally identical and keeps
  // the lint clean.
  let formName = $state('');
  let formURL = $state('');
  let formToken = $state('');
  let formError = $state<string | null>(null);
  let revealFormToken = $state(false);

  $effect(() => {
    formName = initialName;
  });
  $effect(() => {
    formURL = initialURL;
  });
  $effect(() => {
    formToken = initialToken;
  });
  $effect(() => {
    formError = initialError;
  });

  // validateClient returns null on success or a message on error. We
  // mirror the server-side rules (ws://|wss:// + non-empty token) so
  // the user gets fast feedback without the round-trip; the server
  // re-validates on every save.
  function validateClient(rawURL: string, token: string): string | null {
    const trimmedURL = rawURL.trim();
    if (!trimmedURL) return 'URL is required.';
    let parsed: URL;
    try {
      parsed = new URL(trimmedURL);
    } catch {
      return 'URL must start with ws:// or wss://.';
    }
    if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
      return 'URL must start with ws:// or wss://.';
    }
    if (!parsed.host) return 'URL is missing a host.';
    if (!token.trim()) return 'Token is required.';
    return null;
  }

  function handleSubmit(e: SubmitEvent): void {
    e.preventDefault();
    if (saving) return;
    const localErr = validateClient(formURL, formToken);
    if (localErr) {
      formError = localErr;
      return;
    }
    formError = null;
    onSubmit({
      name: formName.trim(),
      url: formURL.trim(),
      token: formToken.trim(),
    });
  }
</script>

<form
  onsubmit={handleSubmit}
  class="space-y-3 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 p-4"
>
  <div class="flex flex-col gap-1">
    <label class="text-[12px] text-fg-muted" for="remote-endpoint-name">Nickname (optional)</label>
    <input
      id="remote-endpoint-name"
      type="text"
      bind:value={formName}
      placeholder="Tailnet machine"
      class="text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-1.5 text-fg focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40"
    />
  </div>
  <div class="flex flex-col gap-1">
    <label class="text-[12px] text-fg-muted" for="remote-endpoint-url">URL</label>
    <input
      id="remote-endpoint-url"
      type="text"
      bind:value={formURL}
      placeholder="ws://host:port/"
      required
      class="text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-1.5 text-fg font-mono focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40"
    />
  </div>
  <div class="flex flex-col gap-1">
    <label class="text-[12px] text-fg-muted" for="remote-endpoint-token">Token</label>
    <div class="flex gap-2">
      <input
        id="remote-endpoint-token"
        type={revealFormToken ? 'text' : 'password'}
        bind:value={formToken}
        required
        class="flex-1 text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-1.5 text-fg font-mono focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40"
      />
      <button
        type="button"
        onclick={() => (revealFormToken = !revealFormToken)}
        class="text-[12px] font-medium rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg hover:border-accent/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
      >
        {revealFormToken ? 'Hide' : 'Show'}
      </button>
    </div>
  </div>
  {#if formError}
    <p role="alert" class="text-[12px] text-error">{formError}</p>
  {/if}
  <div class="flex gap-2">
    <button
      type="submit"
      disabled={saving}
      class="text-[12px] font-medium rounded-[var(--radius-field)] border border-accent/40 bg-accent/10 px-3 py-1.5 text-fg hover:border-accent/60 disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
    >
      {mode === 'add' ? 'Add' : 'Save'}
    </button>
    <button
      type="button"
      onclick={onCancel}
      class="text-[12px] font-medium rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg hover:border-accent/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
    >
      Cancel
    </button>
  </div>
</form>
