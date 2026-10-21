// Refuse writing back a file the seat only saw through a truncated Read (🎯T209).

type ReadView = { fullBytes: number; returnedBytes: number };

const views = new Map<string, ReadView>();

export function recordReadView(abs: string, fullText: string, returnedText: string): void {
  views.set(abs, {
    fullBytes: Buffer.byteLength(fullText, "utf8"),
    returnedBytes: Buffer.byteLength(returnedText, "utf8"),
  });
}

export function clearReadView(abs: string): void {
  views.delete(abs);
}

export function refusalForTruncatedWrite(abs: string, rel: string, proposedUtf8Bytes: number): string | undefined {
  const v = views.get(abs);
  if (!v || v.returnedBytes >= v.fullBytes) return undefined;
  const slack = Math.max(256, Math.floor(v.fullBytes * 0.01));
  const looksWholeFile =
    proposedUtf8Bytes >= v.returnedBytes - slack && proposedUtf8Bytes <= v.returnedBytes + slack;
  const looksPartialClobber = proposedUtf8Bytes < v.fullBytes - slack;
  if (!looksWholeFile && !looksPartialClobber) return undefined;
  return (
    `refusing write to ${rel} — last Read was truncated (${v.returnedBytes} of ${v.fullBytes} bytes); ` +
    `re-read the full file in ranges before replacing it (🎯T209)`
  );
}
