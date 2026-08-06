<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS } from './styles';

  let settings = $derived(getSettings());

  // We snapshot the tracing state on mount so we can tell the user whether
  // flipping the toggle now requires a restart. Tracing is wired up once at
  // startup — there is no safe way to rebuild an OTLP exporter mid-flight
  // without losing in-flight spans.
  const initialTracingEnabled = getSettings().observabilityTracingEnabled;
  let tracingRequiresRestart = $derived(
    settings.observabilityTracingEnabled !== initialTracingEnabled,
  );

  function handleEndpointChange(value: string) {
    void updateSetting('observabilityOtlpEndpoint', value);
  }
</script>

<div class="flex flex-col gap-6">
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
        label="Enable tracing"
        hint="Turn on OTLP trace + metric export."
      >
        <ToggleSwitch
          checked={settings.observabilityTracingEnabled}
          ariaLabel="Toggle OpenTelemetry tracing"
          onToggle={(value) => updateSetting('observabilityTracingEnabled', value)}
        />
      </SettingsField>

      <SettingsField
        label="OTLP endpoint"
        hint="gRPC host:port. Leave blank to use the OTel default (localhost:4317). Only used when tracing is enabled."
        htmlFor="otlp-endpoint"
        stacked
      >
        <input
          id="otlp-endpoint"
          type="text"
          value={settings.observabilityOtlpEndpoint}
          disabled={!settings.observabilityTracingEnabled}
          placeholder="localhost:4317"
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
        label="Enable event replay log"
        hint="Takes effect immediately — no restart needed."
      >
        <ToggleSwitch
          checked={settings.observabilityEventLogEnabled}
          ariaLabel="Toggle Event Replay Log"
          onToggle={(value) => updateSetting('observabilityEventLogEnabled', value)}
        />
      </SettingsField>
    </div>
  </section>
</div>
