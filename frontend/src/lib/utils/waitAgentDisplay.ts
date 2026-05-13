export function stringArrayValue(obj: Record<string, unknown>, key: string): string[] {
  const value = obj[key];
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : [];
}

export function waitAgentRequestedReceiverIds(input: Record<string, unknown>): string[] {
  return stringArrayValue(input, 'requestedReceiverThreadIds');
}

export function waitAgentDisplayReceiverIds(input: Record<string, unknown>): string[] {
  const requested = waitAgentRequestedReceiverIds(input);
  if (requested.length > 0) return requested;
  return stringArrayValue(input, 'receiverThreadIds');
}
