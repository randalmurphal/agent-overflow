<script lang="ts">
  // Settings as a layered overlay: a SIBLING of <PaneHost> in App.svelte, not
  // a surface that replaces the pane strip. The pane tree stays mounted
  // underneath, so opening and closing rebuild nothing.
  //
  // Thin on purpose — the frame is OverlayShell (shared with the workflows
  // overlay) and everything inside is SettingsView. Esc is keybinding-driven
  // (`settings.close`); the scrim and the view's close button route through
  // the same `onClose`, which is where the blur-before-unmount lives.

  import OverlayShell from '../primitives/OverlayShell.svelte';
  import SettingsView from './SettingsView.svelte';
  import type { SettingsSection } from './sections';

  interface Props {
    open: boolean;
    initialSection?: SettingsSection;
    onClose: () => void;
  }

  let { open, initialSection = 'general', onClose }: Props = $props();
</script>

<OverlayShell
  {open}
  ariaLabel="Settings"
  onScrimClick={onClose}
  scrimTestId="settings-overlay-scrim"
  testId="settings-overlay"
>
  <SettingsView {initialSection} {onClose} />
</OverlayShell>
