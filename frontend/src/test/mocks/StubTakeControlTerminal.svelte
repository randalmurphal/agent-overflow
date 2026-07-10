<script lang="ts">
  let { paneId, threadId }: { paneId: string; threadId: string } = $props();
</script>

<div data-testid="stub-take-control-terminal" data-pane-id={paneId} data-thread-id={threadId}>
  Take-control terminal
</div>
