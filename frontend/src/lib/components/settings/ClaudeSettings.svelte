<script lang="ts">
  // The Claude Code page. Its body is the shared provider page — the Claude
  // and Codex pages differ only in which provider they configure, and the
  // Claude-only sections (session axes, cross-session messaging) are gated
  // inside it.

  import ProviderSettingsPage from './ProviderSettingsPage.svelte';
</script>

<ProviderSettingsPage provider="claude" />
