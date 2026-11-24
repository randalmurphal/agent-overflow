// Thread window replica (docs/architecture/thread-replica-sync.md §6).
// Importing this module arms the backend-identity binding in session.ts;
// nothing else has to be wired at startup.
export {
  getReplicaWindow,
  initReplica,
  putReplicaWindow,
  removeReplicaWindow,
} from './session';
export type { ReplicaBody } from './envelope';
