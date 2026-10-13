// The moment a thread stops being a standing agent in the thread list
// (demoted, archived, deleted). A leaf module: the thread store announces
// it and the stores that read the thread store listen, so neither imports
// the other.

let observers: Array<(threadId: string) => void> = [];

/** Run `observer` with the thread id each time a standing agent ends. */
export function onStandingAgentEnded(observer: (threadId: string) => void): () => void {
  observers = [...observers, observer];
  return () => {
    observers = observers.filter((existing) => existing !== observer);
  };
}

export function notifyStandingAgentEnded(threadId: string): void {
  for (const observer of observers) observer(threadId);
}
