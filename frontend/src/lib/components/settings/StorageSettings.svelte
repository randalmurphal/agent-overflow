<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import ArchivedThreads from './ArchivedThreads.svelte';
  import { INPUT_CLASS } from './styles';

  // Mirrors internal/settings.MaxRetentionDays. Hard-capped on the Go
  // side as well; bounding the input here keeps the UI honest and stops
  // a typo from triggering the load-time clamp.
  const MAX_RETENTION_DAYS = 36500;

  let settings = $derived(getSettings());
</script>

<div class="flex flex-col gap-6">
  <section data-testid="settings-retention">
    <SettingsHeader title="Automatic Cleanup" />
    <div class="flex flex-col gap-1">
      <SettingsField
        label="Retention (days)"
        hint={settings.retention.days === 0
          ? 'Automatic cleanup is disabled. Nothing will be removed automatically.'
          : 'Threads (with their attachments, design workdirs, and replay logs), provider-event logs, and bug-report bookmarks older than this are cleaned up automatically. Set to 0 to disable.'}
        htmlFor="retention-days"
      >
        <input
          id="retention-days"
          data-testid="settings-retention-days"
          type="number"
          min="0"
          max={MAX_RETENTION_DAYS}
          step="1"
          value={settings.retention.days}
          onblur={(e) => {
            const raw = (e.target as HTMLInputElement).value;
            const parsed = parseInt(raw, 10);
            let next = Number.isFinite(parsed) && parsed >= 0 ? parsed : 0;
            if (next > MAX_RETENTION_DAYS) next = MAX_RETENTION_DAYS;
            void updateSetting('retention', { days: next });
          }}
          class="{INPUT_CLASS} max-w-[6rem]"
        />
      </SettingsField>
    </div>
  </section>

  <ArchivedThreads />
</div>
