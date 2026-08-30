<script lang="ts">
	// The active trailing literal leaf. The span renders EMPTY —
	// no Svelte-owned Text node — and its controller is the single writer of the
	// element's children, so the parser and the app's streaming reveal can never
	// hold the same visible text run between them. See `literalHost.ts`.
	import {
		attachStreamdownLiteralHost,
		type StreamdownLiteralHostHandle
	} from './literalHost';

	let { text, token }: { text: string; token: unknown } = $props();

	let element: HTMLSpanElement;
	let host: StreamdownLiteralHostHandle | undefined;

	$effect(() => {
		(host ??= attachStreamdownLiteralHost(element)).publish(token, text);
	});

	// Dependency-free, so its teardown runs once, at unmount.
	$effect(() => () => {
		host?.detach();
		host = undefined;
	});
</script>

<span bind:this={element} data-streamdown-direct-append-safe style="display: contents"></span>
