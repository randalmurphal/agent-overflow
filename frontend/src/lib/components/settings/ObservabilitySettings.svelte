<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';

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

<div class="space-y-5">
  <section class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 p-5">
    <div class="mb-4">
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Tracing</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">OpenTelemetry OTLP</h3>
      <p class="mt-1 text-sm text-text-secondary">
        When enabled, Agent Overflow exports spans and metrics to an OTLP-compatible collector
        (Jaeger, Honeycomb, Tempo, etc). Traces capture turn lifecycle, provider I/O, and
        SQLite writes so you can see where time is being spent.
      </p>
    </div>

    {#if tracingRequiresRestart}
      <div
        role="status"
        class="mb-3 rounded-xl border border-amber-400/40 bg-amber-400/10 px-3 py-2 text-xs text-amber-300"
      >
        Tracing changes take effect on next app restart.
      </div>
    {/if}

    <div class="space-y-3">
      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <p class="text-sm text-text-primary">Enable tracing</p>
          <p class="text-xs text-text-secondary/60">Turn on OTLP trace + metric export.</p>
        </div>
        <ToggleSwitch
          checked={settings.observabilityTracingEnabled}
          ariaLabel="Toggle OpenTelemetry tracing"
          onToggle={(value) => updateSetting('observabilityTracingEnabled', value)}
        />
      </div>

      <div class="rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <label for="otlp-endpoint" class="text-sm text-text-primary block">OTLP endpoint</label>
        <p class="mt-1 mb-2 text-xs text-text-secondary/60">
          gRPC host:port. Leave blank to use the OTel default (localhost:4317). Only used
          when tracing is enabled.
        </p>
        <input
          id="otlp-endpoint"
          type="text"
          value={settings.observabilityOtlpEndpoint}
          disabled={!settings.observabilityTracingEnabled}
          placeholder="localhost:4317"
          onchange={(e) => handleEndpointChange((e.target as HTMLInputElement).value)}
          class="w-full text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
        />
      </div>
    </div>
  </section>

  <section class="rounded-[var(--radius-control)] border border-border-subtle bg-card/30 p-5">
    <div class="mb-4">
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Replay log</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">Per-thread event recorder</h3>
      <p class="mt-1 text-sm text-text-secondary">
        Writes every provider event for each thread to <code class="font-mono text-[11px]">~/.agent-overflow/replay/&lt;threadId&gt;.jsonl</code>.
        Useful for reproducing a bad turn after the fact. Files rotate at 100 MB; up to three
        backups are kept.
      </p>
    </div>

    <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
      <div>
        <p class="text-sm text-text-primary">Enable Event Replay Log</p>
        <p class="text-xs text-text-secondary/60">Takes effect immediately — no restart needed.</p>
      </div>
      <ToggleSwitch
        checked={settings.observabilityEventLogEnabled}
        ariaLabel="Toggle Event Replay Log"
        onToggle={(value) => updateSetting('observabilityEventLogEnabled', value)}
      />
    </div>
  </section>
</div>
