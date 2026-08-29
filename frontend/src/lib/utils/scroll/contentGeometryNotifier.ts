export type ContentGeometryListener = (scrollable: boolean) => void;

export interface ContentGeometrySource {
  subscribe(listener: ContentGeometryListener): () => void;
}

export interface ContentGeometryNotifier extends ContentGeometrySource {
  notify(scrollable: boolean): void;
}

/**
 * Imperative fan-out for geometry that an existing owner already measured.
 *
 * Keeping this outside Svelte state matters on streaming surfaces. A height
 * delivery can arrive several times per frame across several panes, while a
 * hidden overlay scrollbar has no work to do. A reactive revision would wake
 * every mounted bar merely to discover that it is hidden.
 */
export function createContentGeometryNotifier(): ContentGeometryNotifier {
  interface Subscription {
    listener: ContentGeometryListener;
    generation: number;
    active: boolean;
  }
  const subscriptions = new Set<Subscription>();
  let generation = 0;

  return {
    subscribe(listener) {
      const subscription: Subscription = {
        listener,
        generation: ++generation,
        active: true,
      };
      subscriptions.add(subscription);
      return () => {
        if (!subscription.active) return;
        subscription.active = false;
        subscriptions.delete(subscription);
      };
    },
    notify(scrollable) {
      // Geometry lands at streaming cadence, usually with one subscriber.
      // Iterate the Set without allocating a snapshot on every delivery.
      // Deleting the current Set entry is iteration-safe. The generation
      // cutoff keeps a listener added by a callback out of this delivery,
      // matching event-source subscription semantics.
      const deliveryGeneration = generation;
      let errors: unknown[] | null = null;
      for (const subscription of subscriptions) {
        if (subscription.generation > deliveryGeneration) break;
        if (!subscription.active) continue;
        try {
          subscription.listener(scrollable);
        } catch (error) {
          (errors ??= []).push(error);
        }
      }
      if (errors) {
        throw new AggregateError(errors, 'content geometry delivery failed');
      }
    },
  };
}
