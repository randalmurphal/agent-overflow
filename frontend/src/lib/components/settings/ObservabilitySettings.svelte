<script lang="ts">
  import { untrack } from 'svelte';
  import { GetObservabilityTracing } from '../../stores/bindings';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { HOST_TIER_REASON, settingsComputer } from './settingsComputer';
  const { call, scopeGrant, getSettings, updateSetting, hostTierWritable } = settingsComputer();
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS } from './styles';

  let settings = $derived(getSettings());
  // All three keys are host tier: they configure THIS backend's exporters
  // and its replay log on disk.
  let hostWritable = $derived(hostTierWritable());
  // The backend leaves the endpoint out of a settings read for a caller
  // without access:admin (internal/app/app_settings.go settingsForCaller),
  // so an empty value here does not mean the default is in use.
  let adminGrant = $derived(scopeGrant('access:admin'));
  let endpointHidden = $derived(adminGrant !== 'granted');

  // Tracing is wired once at start, so a saved value that differs from the
  // one this backend started with waits on a restart. The backend reports
  // that start value (GetObservabilityTracing), so a page opened after a
  // save still shows the change waiting; the saved side is the settings
  // store, which follows every window's writes. No notice while the start
  // value is unknown.
  let launchedWith = $state<boolean | null>(null);
  let tracingRequiresRestart = $derived(
    launchedWith !== null && settings.observabilityTracingEnabled !== launchedWith,
  );

  async function loadLaunchValue(): Promise<void> {
    try {
      launchedWith = (await call(() => GetObservabilityTracing())).launched;
    } catch (err) {
      addErrorToast(`Failed to read the tracing start value: ${errString(err)}`, err);
    }
  }

  // The gate is the only tracked read; see WebviewGPUVsyncField.
  $effect(() => {
    if (scopeGrant('settings:read') !== 'granted') return;
    untrack(() => void loadLaunchValue());
  });

  function handleEndpointChange(value: string) {
    void updateSetting('observabilityOtlpEndpoint', value);
  }
</script>

<div class="settings-sections">
  <section>
    <SettingsHeader
      title="OpenTelemetry OTLP"
      description="When enabled, Agent Overflow exports spans and metrics to an OTLP-compatible collector (Jaeger, Honeycomb, Tempo, etc). Traces capture turn lifecycle, provider I/O, and SQLite writes so you can see where time is being spent."
    />

    <!-- mb, not mt: the header owns the gap above, so the callout owns the
         gap to the fields it displaces — which then need no margin of their
         own whether or not the callout is showing. -->
    {#if tracingRequiresRestart}
      <div class="mb-2.5">
        <SettingsCallout tone="warn">
          Tracing changes take effect on next app restart.
        </SettingsCallout>
      </div>
    {/if}

    <div class="flex flex-col gap-1">
      <SettingsField
        id="observability.tracing"
        label="Enable tracing"
        hint="Turn on OTLP trace + metric export."
      >
        <ToggleSwitch
          checked={settings.observabilityTracingEnabled}
          disabled={!hostWritable}
          title={hostWritable ? undefined : HOST_TIER_REASON}
          ariaLabel="Toggle OpenTelemetry tracing"
          onToggle={(value) => updateSetting('observabilityTracingEnabled', value)}
        />
      </SettingsField>

      <SettingsField
        id="observability.otlp-endpoint"
        label="OTLP endpoint"
        hint={adminGrant === 'refused'
          ? 'This connection cannot read the endpoint. Connect with full access to see it.'
          : grantBlockedTitle(adminGrant) ?? 'gRPC host:port. Leave blank to use the OTel default (localhost:4317). Only used when tracing is enabled.'}
        htmlFor="otlp-endpoint"
        stacked
      >
        <input
          id="otlp-endpoint"
          type="text"
          value={settings.observabilityOtlpEndpoint}
          disabled={!settings.observabilityTracingEnabled || !hostWritable}
          title={hostWritable ? undefined : HOST_TIER_REASON}
          placeholder={endpointHidden ? 'Hidden on this connection' : 'localhost:4317'}
          onchange={(e) => handleEndpointChange((e.target as HTMLInputElement).value)}
          class="{INPUT_CLASS} max-w-md disabled:opacity-50 disabled:cursor-not-allowed"
        />
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader
      title="Per-thread event recorder"
      description="Writes every provider event for each thread to ~/.agent-overflow/replay/<threadId>.jsonl. Useful for reproducing a bad turn after the fact. Files rotate at 100 MB; up to three backups are kept."
    />

    <div class="flex flex-col gap-1">
      <SettingsField
        id="observability.event-log"
        label="Enable event replay log"
        hint="Takes effect immediately — no restart needed."
      >
        <ToggleSwitch
          checked={settings.observabilityEventLogEnabled}
          disabled={!hostWritable}
          title={hostWritable ? undefined : HOST_TIER_REASON}
          ariaLabel="Toggle Event Replay Log"
          onToggle={(value) => updateSetting('observabilityEventLogEnabled', value)}
        />
      </SettingsField>
    </div>
  </section>
</div>
