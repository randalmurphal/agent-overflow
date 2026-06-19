const SENSITIVE_QUERY_PARAM_RE =
  /([?&](?:token|t|access_token|refresh_token|id_token|api[_-]?key|client_secret)=)[^&\s"']+/gi;

export function redactDiagnosticText(value: string): string {
  return value.replace(SENSITIVE_QUERY_PARAM_RE, '$1[redacted]');
}
