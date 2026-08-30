// Thread window replica (docs/architecture/thread-replica-sync.md §6).
// Importing this module arms the backend-identity binding in session.ts;
// nothing else has to be wired at startup.
export {
  getReplicaWindow,
  initReplica,
  putReplicaWindow,
  removeReplicaWindow,
  replicaToken,
  __resetReplicaForTest,
} from './session';
export {
  MAX_ENVELOPE_CHARS,
  MAX_ENVELOPE_ITEMS,
  MAX_REPLICA_CHARS,
  MAX_REPLICA_THREADS,
  REPLICA_ENVELOPE_VERSION,
  REPLICA_SCHEMA_VERSION,
  type ReplicaBody,
  type ReplicaEnvelope,
  type ReplicaStamp,
} from './envelope';
