<script lang="ts">
  // The one provider sign-in surface, shared by Settings → Providers →
  // Accounts and the account-switcher picker. Both are already conditional
  // surfaces with their own chrome, so this is an inline block rather than a
  // modal: a sign-in started from a card belongs under that card, and a second
  // overlay over the picker's overlay would be a dialog inside a dialog.
  //
  // It owns no login logic — start / submit / cancel and the live state all
  // live in stores/providerAccounts.svelte.ts, so a flow begun in one surface
  // is the same flow the other one shows.
  //
  // Three shapes, decided by the state the backend pushes rather than by
  // anything this client remembers:
  //   browser  — the page opened on the backend's own machine; the link stays
  //              visible because "the opener failed" and "open it yourself"
  //              have the same answer.
  //   remote, no code — Claude: open the link wherever you are, approve, paste
  //              the code it shows back here.
  //   remote, with code — Codex: open the link and type the code, which is
  //              displayed at the size of something read off another screen.

  import { onDestroy } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import {
    ProviderLoginPhase,
    ProviderLoginMethod,
    type ProviderLoginState,
  } from '../../stores/bindings';
  import {
    cancelProviderLogin,
    providerLabel as resolveProviderLabel,
    submitProviderLoginCode,
  } from '../../stores/providerAccounts.svelte';
  import type { ProviderID } from '../../types/providers';
  import { handleExternalURL } from '../../utils/externalLinks';

  let { provider, login }: { provider: ProviderID; login: ProviderLoginState } = $props();

  let label = $derived(resolveProviderLabel(provider));
  let phase = $derived(login.phase);
  let pastedCode = $state('');
  let submitting = $state(false);

  let waitingOnBrowser = $derived(phase === ProviderLoginPhase.LoginPhaseAwaitingBrowser);
  let awaitingCode = $derived(phase === ProviderLoginPhase.LoginPhaseAwaitingCode);
  let failed = $derived(phase === ProviderLoginPhase.LoginPhaseFailed);
  let verifying = $derived(phase === ProviderLoginPhase.LoginPhaseVerifying);
  let starting = $derived(phase === ProviderLoginPhase.LoginPhaseStarting);
  // A device code is typed on another screen; Claude's remote flow has none,
  // and its code travels the other way.
  let deviceCode = $derived(login.userCode ?? '');
  let wantsPastedCode = $derived(
    awaitingCode && !deviceCode && login.method === ProviderLoginMethod.LoginMethodRemote,
  );
  let authorizeUrl = $derived(login.authorizeUrl ?? '');

  // The countdown ticks only while a code is on screen with a deadline. A
  // device code with no timer is how somebody sits on a dead one.
  let now = $state(Date.now());
  let ticking = $derived(awaitingCode && !!deviceCode && !!login.expiresAt);
  let tick: ReturnType<typeof setInterval> | undefined;
  $effect(() => {
    if (!ticking) return;
    now = Date.now();
    tick = setInterval(() => {
      now = Date.now();
    }, 1000);
    return () => clearInterval(tick);
  });
  onDestroy(() => clearInterval(tick));

  let remaining = $derived(Math.max(0, (login.expiresAt ?? 0) - now));
  let expiryLine = $derived.by(() => {
    if (!ticking) return '';
    if (remaining <= 0) return 'This code has expired.';
    const totalSeconds = Math.ceil(remaining / 1000);
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    return `This code expires in ${minutes}:${String(seconds).padStart(2, '0')}.`;
  });

  // The link is opened by THIS device whatever the method: a remote reader has
  // to reach the page, and a host reader whose opener failed is exactly who
  // the visible link is for. handleExternalURL already picks the host binding
  // or window.open by run mode.
  function openLink(): void {
    if (authorizeUrl) void handleExternalURL(authorizeUrl);
  }

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    const code = pastedCode.trim();
    if (!code || submitting) return;
    submitting = true;
    try {
      await submitProviderLoginCode(provider, code);
      pastedCode = '';
    } finally {
      submitting = false;
    }
  }
</script>

<div
  class="mt-3 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-3"
  data-testid="provider-login-flow-{provider}"
  data-phase={phase}
  data-method={login.method ?? ''}
>
  {#if failed}
    <p class="text-[0.75rem] font-medium text-error">{label} sign-in did not finish.</p>
    {#if login.error}
      <p class="mt-1 text-[0.6875rem] text-fg-hint" data-testid="provider-login-error">
        {login.error}
      </p>
    {/if}
    <div class="mt-3 flex justify-end">
      <Button variant="ghost" size="xs" onclick={() => void cancelProviderLogin(provider)}>
        Close
      </Button>
    </div>
  {:else}
    <div class="flex items-start justify-between gap-3">
      <div class="min-w-0">
        <p class="text-[0.75rem] font-medium text-fg">Signing in to {label}</p>
        {#if starting}
          <p class="mt-0.5 text-[0.6875rem] text-fg-hint">Starting sign-in…</p>
        {:else if verifying}
          <p class="mt-0.5 text-[0.6875rem] text-fg-hint">Checking with {label}…</p>
        {:else if waitingOnBrowser}
          <p class="mt-0.5 text-[0.6875rem] text-fg-hint">
            Waiting for you to finish signing in in the browser.
          </p>
        {:else if wantsPastedCode}
          <p class="mt-0.5 text-[0.6875rem] text-fg-hint">
            Open this link on the device you are reading this on, approve the sign-in,
            then paste the code it shows back here.
          </p>
        {:else if awaitingCode}
          <p class="mt-0.5 text-[0.6875rem] text-fg-hint">
            Open this link on the device you are reading this on and enter this code.
          </p>
        {/if}
      </div>
      <Button variant="ghost" size="xs" onclick={() => void cancelProviderLogin(provider)}>
        Cancel
      </Button>
    </div>

    <!-- The one prose slot the backend carries. On a live phase it is what
         just changed and why: a link replaced after the provider burned the
         previous one, or a host with no browser to open. -->
    {#if login.error}
      <p class="mt-2 text-[0.6875rem] text-warning" data-testid="provider-login-notice">
        {login.error}
      </p>
    {/if}

    {#if deviceCode}
      <p
        class="mt-3 select-all text-center font-mono text-2xl tracking-[0.35em] text-fg"
        data-testid="provider-login-code"
        aria-label="Sign-in code"
      >
        {deviceCode}
      </p>
      {#if expiryLine}
        <p class="mt-1 text-center text-[0.6875rem] text-fg-hint">{expiryLine}</p>
      {/if}
    {/if}

    {#if authorizeUrl}
      <div class="mt-3 flex items-center gap-2">
        <Button variant="secondary" size="xs" onclick={openLink}>Open the sign-in page</Button>
        <input
          class="min-w-0 flex-1 select-all truncate rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1 font-mono text-[0.6875rem] text-fg-hint"
          value={authorizeUrl}
          readonly
          aria-label="Sign-in link"
          data-testid="provider-login-url"
          onfocus={(event) => event.currentTarget.select()}
        />
      </div>
    {/if}

    {#if wantsPastedCode}
      <form class="mt-3 flex items-center gap-2" onsubmit={submit}>
        <input
          class="min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1 text-[0.6875rem] text-fg placeholder:text-fg-hint"
          bind:value={pastedCode}
          placeholder="Paste the code shown after you approve"
          aria-label="Sign-in code"
          data-testid="provider-login-code-input"
          autocomplete="off"
          spellcheck="false"
        />
        <Button
          variant="primary"
          size="xs"
          type="submit"
          disabled={!pastedCode.trim() || submitting}
        >
          {submitting ? 'Checking…' : 'Submit code'}
        </Button>
      </form>
    {/if}

    {#if waitingOnBrowser || (awaitingCode && deviceCode)}
      <div class="mt-3 flex items-center gap-2 text-[0.6875rem] text-fg-hint">
        <SteppedSpinner size={11} />
        <span>Waiting for you to finish signing in.</span>
      </div>
    {/if}
  {/if}
</div>
