<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';

  let settings = $derived(getSettings());
</script>

<div class="space-y-6">
  <div>
    <h3 class="text-sm font-medium text-text-primary mb-3">Appearance</h3>
    <div class="space-y-3">
      <div class="flex items-center justify-between">
        <div>
          <p class="text-sm text-text-primary">Theme</p>
          <p class="text-xs text-text-secondary/60">Choose your preferred color scheme</p>
        </div>
        <select
          value={settings.theme}
          onchange={(e) => updateSetting('theme', (e.target as HTMLSelectElement).value as 'system' | 'light' | 'dark')}
          class="text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary focus:outline-none focus:border-accent cursor-pointer"
        >
          <option value="system">System</option>
          <option value="light">Light</option>
          <option value="dark">Dark</option>
        </select>
      </div>

      <div class="flex items-center justify-between">
        <div>
          <p class="text-sm text-text-primary">Timestamp format</p>
          <p class="text-xs text-text-secondary/60">How timestamps appear in the chat</p>
        </div>
        <select
          value={settings.timestampFormat}
          onchange={(e) => updateSetting('timestampFormat', (e.target as HTMLSelectElement).value as 'locale' | '12-hour' | '24-hour')}
          class="text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary focus:outline-none focus:border-accent cursor-pointer"
        >
          <option value="locale">System locale</option>
          <option value="12-hour">12-hour</option>
          <option value="24-hour">24-hour</option>
        </select>
      </div>
    </div>
  </div>

  <div class="border-t border-border pt-6">
    <h3 class="text-sm font-medium text-text-primary mb-3">Behavior</h3>
    <div class="space-y-3">
      <label class="flex items-center justify-between cursor-pointer">
        <div>
          <p class="text-sm text-text-primary">Diff word wrap</p>
          <p class="text-xs text-text-secondary/60">Wrap long lines in diff views</p>
        </div>
        <input
          type="checkbox"
          checked={settings.diffWordWrap}
          onchange={() => updateSetting('diffWordWrap', !settings.diffWordWrap)}
          class="w-4 h-4 rounded border-border text-accent focus:ring-accent cursor-pointer"
        />
      </label>

      <label class="flex items-center justify-between cursor-pointer">
        <div>
          <p class="text-sm text-text-primary">Streaming enabled</p>
          <p class="text-xs text-text-secondary/60">Show text as it arrives from the provider</p>
        </div>
        <input
          type="checkbox"
          checked={settings.streamingEnabled}
          onchange={() => updateSetting('streamingEnabled', !settings.streamingEnabled)}
          class="w-4 h-4 rounded border-border text-accent focus:ring-accent cursor-pointer"
        />
      </label>
    </div>
  </div>

  <div class="border-t border-border pt-6">
    <h3 class="text-sm font-medium text-text-primary mb-3">Confirmations</h3>
    <div class="space-y-3">
      <label class="flex items-center justify-between cursor-pointer">
        <div>
          <p class="text-sm text-text-primary">Confirm before archive</p>
          <p class="text-xs text-text-secondary/60">Show a confirmation dialog when archiving threads</p>
        </div>
        <input
          type="checkbox"
          checked={settings.confirmArchive}
          onchange={() => updateSetting('confirmArchive', !settings.confirmArchive)}
          class="w-4 h-4 rounded border-border text-accent focus:ring-accent cursor-pointer"
        />
      </label>

      <label class="flex items-center justify-between cursor-pointer">
        <div>
          <p class="text-sm text-text-primary">Confirm before delete</p>
          <p class="text-xs text-text-secondary/60">Show a confirmation dialog when deleting threads</p>
        </div>
        <input
          type="checkbox"
          checked={settings.confirmDelete}
          onchange={() => updateSetting('confirmDelete', !settings.confirmDelete)}
          class="w-4 h-4 rounded border-border text-accent focus:ring-accent cursor-pointer"
        />
      </label>
    </div>
  </div>
</div>
