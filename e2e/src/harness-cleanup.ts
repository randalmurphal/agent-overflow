const CLEANUP_STAGE_TIMEOUT_MS = 10_000;

export async function boundedCleanup<T>(
  label: string,
  operation: Promise<T>,
  timeoutMs = CLEANUP_STAGE_TIMEOUT_MS,
): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      operation,
      new Promise<T>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error(`${label} timed out after ${timeoutMs}ms`)),
          timeoutMs,
        );
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}
