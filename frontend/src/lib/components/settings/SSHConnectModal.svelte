<script lang="ts">
  import { onDestroy } from 'svelte';
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import { INPUT_CLASS } from './styles';
  import { StartSSHConnection, GetSSHConnection, ConfirmSSHConnection, CancelSSHConnection } from '../../stores/bindings';
  import { addSystem, removeSystem } from '../../stores/systems.svelte';
  import { errString } from '../../utils/errors';
  import { addToast } from '../../stores/toast.svelte';
  import { saveComputerSSH } from '../../stores/computerSSH.svelte';

  let { onClose }: { onClose: () => void } = $props();
  let target = $state('');
  let binary = $state('agent-overflow');
  let startService = $state(false);
  let lan = $state(false);
  let busy = $state(false);
  let status = $state<Awaited<ReturnType<typeof GetSSHConnection>> | null>(null);
  let error = $state('');
  let verification = $state('');
  let confirming = $state(false);
  let disposed = false;
  let generation = 0;
  let attachment = '';
  let confirmed = $state(false);
  let timer: ReturnType<typeof setTimeout> | undefined;

  // A late start/redemption response still owns resources. Retire those even
  // after the dialog disappears; never resurrect UI or retain a pending profile.
  async function cleanup(id: string, attached: string): Promise<void> {
    const results = await Promise.allSettled([
      ...(id ? [Promise.resolve().then(() => CancelSSHConnection(id))] : []),
      ...(attached ? [Promise.resolve().then(() => removeSystem(attached))] : []),
    ]);
    for (const result of results) {
      if (result.status === 'rejected') addToast('error', errString(result.reason));
    }
  }
  function close(): void {
    disposed = true;
    generation++;
    clearTimeout(timer);
    if (!confirmed) void cleanup(status?.id ?? '', attachment);
    onClose();
  }
  onDestroy(() => {
    if (disposed) return;
    disposed = true;
    generation++;
    clearTimeout(timer);
    if (!confirmed) void cleanup(status?.id ?? '', attachment);
  });

  async function poll(id: string, epoch: number): Promise<void> {
    try {
      const next = await GetSSHConnection(id);
      if (disposed || epoch !== generation) return;
      status = next;
      if (next.state === 'error' || next.state === 'canceled') {
        throw new Error(next.error || 'SSH setup was canceled.');
      }
      if (next.state === 'connected') {
        busy = false;
        return;
      }
      if (next.invitation && !attachment) {
        const row = await addSystem(next.invitation);
        if (disposed || epoch !== generation) {
          void cleanup(id, row.id);
          return;
        }
        attachment = row.id;
        verification = row.verificationNumber;
      }
      if (next.state === 'verification' && next.verificationNumber !== verification) {
        throw new Error('The verification numbers do not match. The connection was canceled.');
      }
      timer = setTimeout(() => void poll(id, epoch), 500);
    } catch (err) {
      if (disposed || epoch !== generation) return;
      error = errString(err);
      busy = false;
      const attached = attachment;
      attachment = '';
      if (!confirmed) void cleanup(id, attached);
    }
  }
  async function start(): Promise<void> {
    if (busy || disposed) return;
    error = '';
    status = null;
    verification = '';
    confirmed = false;
    busy = true;
    const epoch = ++generation;
    try {
      const next = await StartSSHConnection({ target: target.trim(), binary: binary.trim(), startService, lan });
      if (disposed || epoch !== generation) {
        void cleanup(next.id, '');
        return;
      }
      status = next;
      void poll(next.id, epoch);
    } catch (err) {
      if (disposed || epoch !== generation) return;
      error = errString(err);
      busy = false;
    }
  }
  async function confirm(): Promise<void> {
    if (!status || confirming || status.state !== 'verification' || verification !== status.verificationNumber) return;
    confirming = true;
    // The remote may accept before our RPC response arrives. Preserve its
    // profile if this window closes during that interval; activation owns it.
    confirmed = true;
    if (attachment) saveComputerSSH(attachment, { target: target.trim(), binary: binary.trim() });
    try {
      await ConfirmSSHConnection(status.id, verification);
    } catch (err) {
      if (!disposed) error = errString(err);
    } finally {
      if (!disposed) confirming = false;
    }
  }
</script>

<Modal open title="Connect over SSH" onClose={close} width="md">
  {#if status?.state === 'connected'}
    <p class="text-sm text-fg">Connected to {target}. Its projects and conversations will appear in the sidebar.</p>
    <div class="mt-4 flex justify-end"><Button variant="primary" onclick={close}>Done</Button></div>
  {:else}
    <form class="flex flex-col gap-4" onsubmit={(event) => { event.preventDefault(); void start(); }}>
      <label class="flex flex-col gap-1.5 text-xs text-fg-muted">
        SSH host
        <input class={INPUT_CLASS} bind:value={target} placeholder="gpu or user@hostname" autocomplete="off" spellcheck={false} disabled={busy} />
      </label>
      <p class="text-xs leading-snug text-fg-muted">Uses this computer’s SSH configuration and keys. Agent Overflow must already be installed on the other computer.</p>
      <details class="text-xs text-fg-muted">
        <summary class="cursor-pointer">Startup &amp; network</summary>
        <div class="mt-3 flex flex-col gap-3">
          <label class="flex flex-col gap-1.5">
            Remote executable
            <input class={INPUT_CLASS} bind:value={binary} spellcheck={false} disabled={busy} />
          </label>
          <label class="flex items-center gap-2"><input type="checkbox" bind:checked={startService} disabled={busy} />Start its installed background service</label>
          <label class="flex items-center gap-2"><input type="checkbox" bind:checked={lan} disabled={busy} />Enable connections on its local network</label>
        </div>
      </details>
      {#if error}<p role="alert" class="whitespace-pre-wrap break-words text-xs text-danger">{error}</p>{/if}
      {#if status?.state === 'verification' && verification && !error}
        <div class="rounded-[var(--radius-field)] border border-border-subtle px-3 py-3">
          <p class="text-xs text-fg-muted">{target} and this app show the same verification number.</p>
          <p aria-label="SSH verification number" class="my-2 text-xl font-semibold tracking-[0.2em] tabular-nums text-fg">{verification}</p>
          <Button variant="primary" disabled={confirming || confirmed} onclick={() => void confirm()}>{confirmed ? 'Connecting…' : 'Connect this computer'}</Button>
        </div>
      {:else if busy}
        <p role="status" class="text-xs text-fg-muted">{status?.state === 'confirming' ? 'Completing pairing…' : `Connecting to ${target}…`}</p>
      {/if}
      <div class="flex justify-end gap-2">
        <Button variant="ghost" onclick={close}>{confirmed ? 'Close' : 'Cancel'}</Button>
        {#if !busy}<Button type="submit" variant="primary" disabled={!target.trim() || !binary.trim()}>{error ? 'Try again' : 'Continue'}</Button>{/if}
      </div>
    </form>
  {/if}
</Modal>
