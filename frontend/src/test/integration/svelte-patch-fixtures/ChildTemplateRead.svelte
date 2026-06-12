<script lang="ts">
  // Healthy path: the defaulted prop IS read from the template, so a
  // connected reader subscribes the whole chain (template effect →
  // prop wrapper → parent memo → pane derived → pane signal). This is
  // the positive control: it proves the `reactions` introspection in
  // the test observes real subscriptions, that the patched runtime
  // still connects chains read by connected readers, and that unmount
  // fully disconnects them.
  interface Props {
    workspacePath?: string;
    title: string;
    onInit: (workspacePath: string) => void;
  }
  let { workspacePath = '', title, onInit }: Props = $props();
  // svelte-ignore state_referenced_locally
  onInit(workspacePath);
</script>

<div>{title}: {workspacePath}</div>
