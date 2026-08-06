<script lang="ts">
  interface Props {
    item: number;
    readDep: () => number;
    throwOn: () => number[];
    onteardown: (item: number) => void;
  }
  let { item, readDep, throwOn, onteardown }: Props = $props();

  $effect(() => {
    // subscribe to the shared dep so a stranded effect stays observable as a
    // leftover entry in its `reactions`
    readDep();
    return () => {
      onteardown(item);
      if (throwOn().includes(item)) throw new Error(`teardown boom ${item}`);
    };
  });
</script>

<span>{item}</span>
