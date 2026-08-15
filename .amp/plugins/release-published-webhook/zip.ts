import { inflateRawSync } from "node:zlib";

const MAX_ARCHIVE = 4_000_000;
const MAX_FILE = 1_000_000;
const MAX_OUTPUT = 4_000_000;
const MAX_FILES = 16;
const decoder = new TextDecoder("utf-8", { fatal: true });

export class InvalidZip extends Error {}

function crc32(bytes: Uint8Array): number {
  let crc = 0xffffffff;
  for (const byte of bytes) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1));
  }
  return (crc ^ 0xffffffff) >>> 0;
}

interface Entry { name: string; basename: string; flags: number; method: number; crc: number; compressed: number; uncompressed: number; local: number }

/** A deliberately small reader for the constrained ZIPs emitted by Actions artifacts. */
export function readZipBasenames(input: Uint8Array): Map<string, Uint8Array> {
  if (!input.length || input.length > MAX_ARCHIVE) throw new InvalidZip("ZIP size is invalid");
  const b = Buffer.from(input);
  let eocd = -1;
  for (let i = Math.max(0, b.length - 65_557); i <= b.length - 22; i++) if (b.readUInt32LE(i) === 0x06054b50) eocd = i;
  if (eocd < 0 || eocd + 22 + b.readUInt16LE(eocd + 20) !== b.length) throw new InvalidZip("ZIP directory is malformed");
  const count = b.readUInt16LE(eocd + 10), directorySize = b.readUInt32LE(eocd + 12), directoryOffset = b.readUInt32LE(eocd + 16);
  if (b.readUInt16LE(eocd + 4) || b.readUInt16LE(eocd + 6) || count !== b.readUInt16LE(eocd + 8) || count > MAX_FILES ||
      count === 0xffff || directorySize === 0xffffffff || directoryOffset === 0xffffffff || directoryOffset + directorySize !== eocd)
    throw new InvalidZip("ZIP layout is unsupported");

  const entries: Entry[] = [], paths = new Set<string>(), basenames = new Set<string>();
  let totalOutput = 0, p = directoryOffset;
  for (let n = 0; n < count; n++) {
    if (p + 46 > eocd || b.readUInt32LE(p) !== 0x02014b50) throw new InvalidZip("ZIP central header is malformed");
    const flags = b.readUInt16LE(p + 8), method = b.readUInt16LE(p + 10), crc = b.readUInt32LE(p + 16);
    const compressed = b.readUInt32LE(p + 20), uncompressed = b.readUInt32LE(p + 24);
    const nameLength = b.readUInt16LE(p + 28), extraLength = b.readUInt16LE(p + 30), commentLength = b.readUInt16LE(p + 32);
    const disk = b.readUInt16LE(p + 34), local = b.readUInt32LE(p + 42), end = p + 46 + nameLength + extraLength + commentLength;
    if (![0, 0x8, 0x800, 0x808].includes(flags) || (method !== 0 && method !== 8) || disk || extraLength || commentLength ||
        compressed === 0xffffffff || uncompressed === 0xffffffff || uncompressed > MAX_FILE || end > eocd)
      throw new InvalidZip("ZIP entry is unsupported");
    let name: string;
    try { name = decoder.decode(b.subarray(p + 46, p + 46 + nameLength)); } catch { throw new InvalidZip("ZIP name encoding is invalid"); }
    const basename = name.slice(name.lastIndexOf("/") + 1);
    if (!name || name.includes("\0") || name.startsWith("/") || name.includes("\\") || name.split("/").some((x) => !x || x === "." || x === "..") || paths.has(name))
      throw new InvalidZip("ZIP path is ambiguous");
    if (basenames.has(basename)) throw new InvalidZip("ZIP basename is duplicated");
    paths.add(name); basenames.add(basename);
    totalOutput += uncompressed;
    if (totalOutput > MAX_OUTPUT) throw new InvalidZip("ZIP aggregate output is too large");
    entries.push({ name, basename, flags, method, crc, compressed, uncompressed, local });
    p = end;
  }
  if (p !== eocd) throw new InvalidZip("ZIP directory length differs");

  const result = new Map<string, Uint8Array>();
  let cursor = 0;
  for (const entry of [...entries].sort((a, c) => a.local - c.local)) {
    const { local, flags, method, compressed, uncompressed, crc, name, basename } = entry;
    if (local !== cursor || local + 30 > directoryOffset || b.readUInt32LE(local) !== 0x04034b50 || b.readUInt16LE(local + 6) !== flags || b.readUInt16LE(local + 8) !== method)
      throw new InvalidZip("ZIP local records are not contiguous");
    const localCRC = b.readUInt32LE(local + 14), localCompressed = b.readUInt32LE(local + 18), localUncompressed = b.readUInt32LE(local + 22);
    const nameLength = b.readUInt16LE(local + 26), extraLength = b.readUInt16LE(local + 28);
    if (extraLength) throw new InvalidZip("ZIP local entry is unsupported");
    let localName: string;
    try { localName = decoder.decode(b.subarray(local + 30, local + 30 + nameLength)); } catch { throw new InvalidZip("ZIP name encoding is invalid"); }
    const start = local + 30 + nameLength;
    if (localName !== name || start + compressed > directoryOffset) throw new InvalidZip("ZIP local entry differs");
    cursor = start + compressed;
    if (flags & 8) {
      if (localCRC !== 0 || localCompressed !== 0 || localUncompressed !== 0)
        throw new InvalidZip("ZIP local descriptor metadata differs");
      const matches = (at: number) => at + 12 <= directoryOffset && b.readUInt32LE(at) === crc && b.readUInt32LE(at + 4) === compressed && b.readUInt32LE(at + 8) === uncompressed;
      const signed = cursor + 16 <= directoryOffset && b.readUInt32LE(cursor) === 0x08074b50 && matches(cursor + 4);
      const descriptor = signed ? cursor + 4 : cursor;
      if (!matches(descriptor))
        throw new InvalidZip("ZIP data descriptor differs");
      cursor = descriptor + 12;
    } else if (localCRC !== crc || localCompressed !== compressed || localUncompressed !== uncompressed) {
      throw new InvalidZip("ZIP local metadata differs");
    }
    let data: Uint8Array;
    try { data = method === 0 ? new Uint8Array(b.subarray(start, start + compressed)) : new Uint8Array(inflateRawSync(b.subarray(start, start + compressed), { maxOutputLength: MAX_FILE })); }
    catch { throw new InvalidZip("ZIP deflate stream is invalid"); }
    if (data.length !== uncompressed || crc32(data) !== crc || (method === 0 && compressed !== uncompressed)) throw new InvalidZip("ZIP entry checksum differs");
    result.set(basename, data);
  }
  if (cursor !== directoryOffset) throw new InvalidZip("ZIP local records are not contiguous");
  return result;
}
