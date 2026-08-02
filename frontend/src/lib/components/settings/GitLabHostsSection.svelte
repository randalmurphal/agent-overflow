<script lang="ts">
  // Mirrors internal/settings.validateBareHostname: client + server must
  // reject the same shapes so the UI never surfaces "looks fine" for an
  // input the strict-update path would refuse.

  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, GHOST_BUTTON_CLASS } from './styles';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  let settings = $derived(getSettings());
  let hosts = $derived(settings.gitlabSelfHostedHosts ?? []);

  let candidate = $state('');
  let candidateError = $derived(validateCandidate(candidate, hosts));
  let canAdd = $derived(candidate.trim() !== '' && candidateError === null);

  function validateCandidate(raw: string, current: string[]): string | null {
    const host = raw.trim().toLowerCase();
    if (host === '') return null;
    if (host.includes('://')) return 'Enter only the hostname, without https:// or other scheme.';
    if (/[\s/?#@:]/.test(host)) return 'Enter only the hostname — no slashes, ports, or userinfo.';
    if (!/^[a-z0-9.-]+$/.test(host)) return 'Hostname may only contain letters, digits, dots, and hyphens.';
    if (host.startsWith('.') || host.endsWith('.')) return 'Hostname must not start or end with a dot.';
    if (host.startsWith('-') || host.endsWith('-')) return 'Hostname must not start or end with a hyphen.';
    if (host.includes('..')) return 'Hostname must not contain consecutive dots.';
    if (!host.includes('.')) return 'Hostname must contain at least one dot (e.g. gitlab.mycompany.com).';
    if (host === 'github.com' || host === 'gitlab.com') {
      return `${host} is already recognised — no configuration needed.`;
    }
    if (current.includes(host)) return 'This host is already on the list.';
    return null;
  }

  async function addHost(): Promise<void> {
    if (!canAdd) return;
    const host = candidate.trim().toLowerCase();
    const next = [...hosts, host];
    await updateSetting('gitlabSelfHostedHosts', next);
    candidate = '';
  }

  async function removeHost(host: string): Promise<void> {
    const next = hosts.filter((h) => h !== host);
    await updateSetting('gitlabSelfHostedHosts', next);
  }

  function handleKeydown(e: KeyboardEvent): void {
    // Enter confirms the IME candidate while composing; adding here would
    // persist the pre-composition host string.
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter' && canAdd) {
      e.preventDefault();
      void addHost();
    }
  }
</script>

<section data-testid="settings-gitlab-hosts">
  <SettingsHeader
    eyebrow="Git Forges"
    title="Self-hosted GitLab Hosts"
    description="Hostnames that should be treated as GitLab when classifying a repo's origin remote. Enables Create MR, MR labels, and the `glab` CLI for self-hosted instances. gitlab.com and github.com are recognised automatically."
  />

  <div class="mt-4 flex flex-col gap-3">
    <div class="flex items-start gap-2">
      <div class="flex-1">
        <label for="gitlab-host-input" class="sr-only">Self-hosted GitLab hostname</label>
        <input
          id="gitlab-host-input"
          data-testid="settings-gitlab-host-input"
          type="text"
          value={candidate}
          placeholder="gitlab.mycompany.com"
          autocomplete="off"
          spellcheck="false"
          oninput={(e) => (candidate = (e.target as HTMLInputElement).value)}
          onkeydown={handleKeydown}
          aria-invalid={candidateError !== null}
          aria-describedby={candidateError ? 'gitlab-host-error' : undefined}
          class="{INPUT_CLASS} max-w-[24rem]"
        />
      </div>
      <button
        type="button"
        data-testid="settings-gitlab-host-add"
        onclick={() => void addHost()}
        disabled={!canAdd}
        class={PRIMARY_BUTTON_CLASS}
      >
        Add
      </button>
    </div>

    {#if candidateError}
      <p
        id="gitlab-host-error"
        data-testid="settings-gitlab-host-error"
        class="text-[0.71875rem] text-error"
        role="alert"
      >
        {candidateError}
      </p>
    {/if}

    {#if hosts.length === 0}
      <p class="text-[0.71875rem] text-fg-hint" data-testid="settings-gitlab-hosts-empty">
        No self-hosted GitLab hosts configured.
      </p>
    {:else}
      <ul class="flex flex-col gap-1" data-testid="settings-gitlab-hosts-list">
        {#each hosts as host (host)}
          <li
            class="flex items-center justify-between gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/30 px-3 py-1.5"
            data-testid="settings-gitlab-host-row-{host}"
          >
            <span class="font-mono text-[0.75rem] text-fg">{host}</span>
            <button
              type="button"
              data-testid="settings-gitlab-host-remove-{host}"
              onclick={() => void removeHost(host)}
              class={GHOST_BUTTON_CLASS}
              aria-label={`Remove ${host}`}
            >
              Remove
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
</section>
