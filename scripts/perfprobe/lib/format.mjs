// Small output helpers shared by the probes.
export const pad = (v, n = 8) => String(v).padStart(n);
export const ms = (us) => (us / 1000).toFixed(1);
export const mb = (bytes) => (bytes / 1048576).toFixed(1);
// Expected failures print one line and exit, no stack trace.
export function fail(msg) {
  console.error(msg);
  process.exit(1);
}
