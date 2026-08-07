import { getContext, onMount, setContext } from 'svelte';
export class StreamdownContext {
    footnotes = {
        refs: new Map(),
        footnotes: new Map()
    };
    isMounted = false;
    // Count of async resources currently in-flight: highlighter loads,
    // katex module import, mermaid module import / render. Each call to
    // `registerAsyncResource()` increments and returns a one-shot resolve
    // function that decrements. Reactive so consumers (Streamdown.svelte's
    // onsettled wiring, embedding apps) can subscribe via $effect.
    pendingAsyncCount = $state(0);
    registerAsyncResource() {
        this.pendingAsyncCount += 1;
        let done = false;
        return () => {
            if (done)
                return;
            done = true;
            this.pendingAsyncCount -= 1;
        };
    }
    get animationTextStyle() {
        return getContext('POPOVER')
            ? undefined
            : this.animation.enabled
                ? `animation-name: sd-${this.animation.type};
animation-duration: ${this.animation.duration}ms;
animation-timing-function: ${this.animation.timingFunction};
animation-iteration-count: 1;
animation-fill-mode: forwards;
white-space: pre-wrap;
display: inline-block;
text-decoration: inherit;`
                : undefined;
    }
    get animationBlockStyle() {
        return getContext('POPOVER')
            ? undefined
            : this.animation.enabled
                ? `animation-name: sd-${this.animation.type};
animation-duration: ${this.animation.duration}ms;
animation-timing-function: ${this.animation.timingFunction};
animation-iteration-count: 1;
animation-fill-mode: forwards;`
                : undefined;
    }
    constructor(props) {
        bind(this, props);
        setContext('streamdown', this);
        if (this.animation.animateOnMount) {
            this.isMounted = true;
        }
        onMount(() => {
            this.isMounted = true;
        });
        $effect(() => {
            this.isMounted = this.animation.enabled;
        });
    }
}
export const useStreamdown = () => {
    const context = getContext('streamdown');
    if (!context) {
        throw new Error('Streamdown context not found');
    }
    return context;
};
import { bind } from './utils/bind.js';
