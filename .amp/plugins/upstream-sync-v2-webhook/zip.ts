import { inflateRawSync } from 'node:zlib'

const maximumArchiveBytes = 4_000_000
const maximumFileBytes = 1_000_000
const maximumOutputBytes = 4_000_000
const maximumFiles = 64
const decoder = new TextDecoder('utf-8', { fatal: true })

export class InvalidZip extends Error {}

function crc32(bytes: Uint8Array): number {
  let crc = 0xffffffff
  for (const byte of bytes) {
    crc ^= byte
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1))
  }
  return (crc ^ 0xffffffff) >>> 0
}

interface Entry {
  name: string
  basename: string
  flags: number
  method: number
  crc: number
  compressed: number
  uncompressed: number
  local: number
}

/** A deliberately small reader for the constrained ZIPs emitted by Actions artifacts. */
export function readZipBasenames(input: Uint8Array): Map<string, Uint8Array> {
  if (!input.length || input.length > maximumArchiveBytes) throw new InvalidZip('ZIP size is invalid')
  const bytes = Buffer.from(input)
  let endOfCentralDirectory = -1
  for (let index = Math.max(0, bytes.length - 65_557); index <= bytes.length - 22; index++) {
    if (bytes.readUInt32LE(index) === 0x06054b50) endOfCentralDirectory = index
  }
  if (
    endOfCentralDirectory < 0 ||
    endOfCentralDirectory + 22 + bytes.readUInt16LE(endOfCentralDirectory + 20) !== bytes.length
  ) {
    throw new InvalidZip('ZIP directory is malformed')
  }
  const count = bytes.readUInt16LE(endOfCentralDirectory + 10)
  const directorySize = bytes.readUInt32LE(endOfCentralDirectory + 12)
  const directoryOffset = bytes.readUInt32LE(endOfCentralDirectory + 16)
  if (
    bytes.readUInt16LE(endOfCentralDirectory + 4) ||
    bytes.readUInt16LE(endOfCentralDirectory + 6) ||
    count !== bytes.readUInt16LE(endOfCentralDirectory + 8) ||
    count > maximumFiles ||
    count === 0xffff ||
    directorySize === 0xffffffff ||
    directoryOffset === 0xffffffff ||
    directoryOffset + directorySize !== endOfCentralDirectory
  ) {
    throw new InvalidZip('ZIP layout is unsupported')
  }

  const entries: Entry[] = []
  const paths = new Set<string>()
  const basenames = new Set<string>()
  let totalOutput = 0
  let position = directoryOffset
  for (let entryIndex = 0; entryIndex < count; entryIndex++) {
    if (position + 46 > endOfCentralDirectory || bytes.readUInt32LE(position) !== 0x02014b50) {
      throw new InvalidZip('ZIP central header is malformed')
    }
    const flags = bytes.readUInt16LE(position + 8)
    const method = bytes.readUInt16LE(position + 10)
    const crc = bytes.readUInt32LE(position + 16)
    const compressed = bytes.readUInt32LE(position + 20)
    const uncompressed = bytes.readUInt32LE(position + 24)
    const nameLength = bytes.readUInt16LE(position + 28)
    const extraLength = bytes.readUInt16LE(position + 30)
    const commentLength = bytes.readUInt16LE(position + 32)
    const disk = bytes.readUInt16LE(position + 34)
    const local = bytes.readUInt32LE(position + 42)
    const end = position + 46 + nameLength + extraLength + commentLength
    if (
      ![0, 0x8, 0x800, 0x808].includes(flags) ||
      (method !== 0 && method !== 8) ||
      disk ||
      extraLength ||
      commentLength ||
      compressed === 0xffffffff ||
      uncompressed === 0xffffffff ||
      uncompressed > maximumFileBytes ||
      end > endOfCentralDirectory
    ) {
      throw new InvalidZip('ZIP entry is unsupported')
    }
    let name: string
    try {
      name = decoder.decode(bytes.subarray(position + 46, position + 46 + nameLength))
    } catch {
      throw new InvalidZip('ZIP name encoding is invalid')
    }
    const basename = name.slice(name.lastIndexOf('/') + 1)
    if (
      !name ||
      name.includes('\0') ||
      name.startsWith('/') ||
      name.includes('\\') ||
      name.split('/').some((segment) => !segment || segment === '.' || segment === '..') ||
      paths.has(name)
    ) {
      throw new InvalidZip('ZIP path is ambiguous')
    }
    if (basenames.has(basename)) throw new InvalidZip('ZIP basename is duplicated')
    paths.add(name)
    basenames.add(basename)
    totalOutput += uncompressed
    if (totalOutput > maximumOutputBytes) throw new InvalidZip('ZIP aggregate output is too large')
    entries.push({ name, basename, flags, method, crc, compressed, uncompressed, local })
    position = end
  }
  if (position !== endOfCentralDirectory) throw new InvalidZip('ZIP directory length differs')

  const result = new Map<string, Uint8Array>()
  let cursor = 0
  for (const entry of [...entries].sort((left, right) => left.local - right.local)) {
    const { local, flags, method, compressed, uncompressed, crc, name, basename } = entry
    if (
      local !== cursor ||
      local + 30 > directoryOffset ||
      bytes.readUInt32LE(local) !== 0x04034b50 ||
      bytes.readUInt16LE(local + 6) !== flags ||
      bytes.readUInt16LE(local + 8) !== method
    ) {
      throw new InvalidZip('ZIP local records are not contiguous')
    }
    const localCRC = bytes.readUInt32LE(local + 14)
    const localCompressed = bytes.readUInt32LE(local + 18)
    const localUncompressed = bytes.readUInt32LE(local + 22)
    const nameLength = bytes.readUInt16LE(local + 26)
    const extraLength = bytes.readUInt16LE(local + 28)
    if (extraLength) throw new InvalidZip('ZIP local entry is unsupported')
    let localName: string
    try {
      localName = decoder.decode(bytes.subarray(local + 30, local + 30 + nameLength))
    } catch {
      throw new InvalidZip('ZIP name encoding is invalid')
    }
    const start = local + 30 + nameLength
    if (localName !== name || start + compressed > directoryOffset) {
      throw new InvalidZip('ZIP local entry differs')
    }
    cursor = start + compressed
    if (flags & 8) {
      if (localCRC !== 0 || localCompressed !== 0 || localUncompressed !== 0) {
        throw new InvalidZip('ZIP local descriptor metadata differs')
      }
      const matches = (at: number) =>
        at + 12 <= directoryOffset &&
        bytes.readUInt32LE(at) === crc &&
        bytes.readUInt32LE(at + 4) === compressed &&
        bytes.readUInt32LE(at + 8) === uncompressed
      const signed =
        cursor + 16 <= directoryOffset &&
        bytes.readUInt32LE(cursor) === 0x08074b50 &&
        matches(cursor + 4)
      const descriptor = signed ? cursor + 4 : cursor
      if (!matches(descriptor)) throw new InvalidZip('ZIP data descriptor differs')
      cursor = descriptor + 12
    } else if (localCRC !== crc || localCompressed !== compressed || localUncompressed !== uncompressed) {
      throw new InvalidZip('ZIP local metadata differs')
    }
    let data: Uint8Array
    try {
      data = method === 0
        ? new Uint8Array(bytes.subarray(start, start + compressed))
        : new Uint8Array(inflateRawSync(bytes.subarray(start, start + compressed), { maxOutputLength: maximumFileBytes }))
    } catch {
      throw new InvalidZip('ZIP deflate stream is invalid')
    }
    if (
      data.length !== uncompressed ||
      crc32(data) !== crc ||
      (method === 0 && compressed !== uncompressed)
    ) {
      throw new InvalidZip('ZIP entry checksum differs')
    }
    result.set(basename, data)
  }
  if (cursor !== directoryOffset) throw new InvalidZip('ZIP local records are not contiguous')
  return result
}
