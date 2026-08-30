import { getContext, setContext } from 'svelte';
export class StreamdownContext {
    footnotes = {
        refs: new Map(),
        footnotes: new Map()
    };
    // Count of async resources currently in-flight: katex module import,
    // mermaid module import / render, host-owned code highlighting. Each call to
    // `registerAsyncResource()` increments and returns a one-shot resolve
    // function that decrements. Reactive so consumers (Streamdown.svelte's
    // onsettled wiring, embedding apps) can subscribe via $effect.
    pendingAsyncCount = $state(0);
    staticRetryGeneration = 0;
    staticRetryListeners = new Set();
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
    requestStaticRetry() {
        this.staticRetryGeneration += 1;
        for (const listener of this.staticRetryListeners)
            listener();
    }
    registerStaticRetry(listener) {
        this.staticRetryListeners.add(listener);
        if (this.staticRetryGeneration > 0)
            listener();
        let released = false;
        return () => {
            if (released)
                return;
            released = true;
            this.staticRetryListeners.delete(listener);
        };
    }
    constructor(props) {
        bind(this, props);
        setContext('streamdown', this);
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
