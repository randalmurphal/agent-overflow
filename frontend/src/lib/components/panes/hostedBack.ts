// A tool pane whose header Back is the one Back (the Pull requests page on
// compact) tells the views it hosts, so a view with its own Back control
// (a CI log, the conflict view) drops it and leaves the step to the
// header, whose ladder closes that view first. Read through context, so a
// view hosted anywhere else keeps its control.

import { getContext, setContext } from 'svelte';

const HOSTED_BACK = Symbol('hosted-back');

/** Called by the host during init; `owns` is read reactively. */
export function provideHostedBack(owns: () => boolean): void {
  setContext(HOSTED_BACK, owns);
}

/** Called by a hosted view during init: answers whether its host's
 * header owns Back now. */
export function hostedBackReader(): () => boolean {
  return getContext<(() => boolean) | undefined>(HOSTED_BACK) ?? (() => false);
}
