/**
 * Compression ratio (raw / compressed), guarded against the cold-start case
 * where `compressedBytes` is still 0 — dividing there yields Infinity, which
 * previously rendered as "Infinityx". Returns 0 when the ratio is undefined,
 * which callers format as a neutral placeholder.
 */
export function compressionRatio(rawBytes: number, compressedBytes: number): number {
  if (!Number.isFinite(rawBytes) || !Number.isFinite(compressedBytes)) return 0;
  if (rawBytes <= 0 || compressedBytes <= 0) return 0;
  return rawBytes / compressedBytes;
}
