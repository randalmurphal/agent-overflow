// Fake for `bindings/agent-overflow/internal/provider/models.js`.
//
// The real module exports class constructors the frontend uses to shape RPC
// parameters. For tests we just need classes whose instances carry the fields
// so assertions can compare them structurally.

export class ApprovalResponse {
  requestId: string;
  decision: string;
  input: unknown;
  constructor(data: Partial<ApprovalResponse> = {}) {
    this.requestId = data.requestId ?? '';
    this.decision = data.decision ?? '';
    this.input = data.input ?? undefined;
  }
}

export class PermissionProfile {
  network?: { enabled?: boolean };
  fileSystem?: { read?: string[]; write?: string[] };
  constructor(data: Partial<PermissionProfile> = {}) {
    this.network = data.network;
    this.fileSystem = data.fileSystem;
  }
}
