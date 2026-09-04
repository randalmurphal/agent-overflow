<script lang="ts">
  // Stands in for any component whose render throws, so a boundary
  // test can stage the failure without a real bug to reproduce. It
  // declares no props: whatever the mocked component took is ignored.
  throw new Error('fixture render failure');
</script>
