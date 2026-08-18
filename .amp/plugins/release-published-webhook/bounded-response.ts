export type ResponseLimitError = (message: string) => Error;

export async function readBoundedResponse(
  response: Response,
  maximumBytes: number,
  label: string,
  error: ResponseLimitError,
): Promise<Uint8Array> {
  if (!Number.isSafeInteger(maximumBytes) || maximumBytes < 1)
    throw error(`${label} size limit is invalid`);

  const declared = response.headers.get("content-length");
  if (
    declared !== null &&
    (!/^(?:0|[1-9][0-9]*)$/.test(declared) ||
      BigInt(declared) > BigInt(maximumBytes))
  ) {
    throw error(`${label} size is invalid`);
  }

  if (!response.body) return new Uint8Array();
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > maximumBytes) {
      try {
        await reader.cancel();
      } catch {}
      throw error(`${label} size is invalid`);
    }
    chunks.push(value);
  }

  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}
