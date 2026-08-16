import { expect, test } from 'bun:test'
import { deflateRawSync } from 'node:zlib'
import { InvalidZip, readZipBasenames } from './zip'

function crc32(value: Uint8Array): number {
  let crc = 0xffffffff
  for (const byte of value) {
    crc ^= byte
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1))
  }
  return (crc ^ 0xffffffff) >>> 0
}

function zip(
  files: Array<[string, string, 0 | 8]>,
  descriptor: 'none' | 'signed' | 'unsigned' = 'none',
): Uint8Array {
  const local: Buffer[] = []
  const central: Buffer[] = []
  let offset = 0
  for (const [name, text, method] of files) {
    const nameBytes = Buffer.from(name)
    const raw = Buffer.from(text)
    const data = method ? deflateRawSync(raw) : raw
    const checksum = crc32(raw)
    const flags = descriptor === 'none' ? 0 : 8
    const header = Buffer.alloc(30 + nameBytes.length)
    header.writeUInt32LE(0x04034b50)
    header.writeUInt16LE(20, 4)
    header.writeUInt16LE(flags, 6)
    header.writeUInt16LE(method, 8)
    if (!flags) {
      header.writeUInt32LE(checksum, 14)
      header.writeUInt32LE(data.length, 18)
      header.writeUInt32LE(raw.length, 22)
    }
    header.writeUInt16LE(nameBytes.length, 26)
    nameBytes.copy(header, 30)
    const footer = Buffer.alloc(descriptor === 'signed' ? 16 : descriptor === 'unsigned' ? 12 : 0)
    let footerOffset = 0
    if (descriptor === 'signed') {
      footer.writeUInt32LE(0x08074b50)
      footerOffset = 4
    }
    if (flags) {
      footer.writeUInt32LE(checksum, footerOffset)
      footer.writeUInt32LE(data.length, footerOffset + 4)
      footer.writeUInt32LE(raw.length, footerOffset + 8)
    }
    local.push(header, data, footer)

    const directory = Buffer.alloc(46 + nameBytes.length)
    directory.writeUInt32LE(0x02014b50)
    directory.writeUInt16LE(20, 4)
    directory.writeUInt16LE(20, 6)
    directory.writeUInt16LE(flags, 8)
    directory.writeUInt16LE(method, 10)
    directory.writeUInt32LE(checksum, 16)
    directory.writeUInt32LE(data.length, 20)
    directory.writeUInt32LE(raw.length, 24)
    directory.writeUInt16LE(nameBytes.length, 28)
    directory.writeUInt32LE(offset, 42)
    nameBytes.copy(directory, 46)
    central.push(directory)
    offset += header.length + data.length + footer.length
  }
  const directory = Buffer.concat(central)
  const end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50)
  end.writeUInt16LE(files.length, 8)
  end.writeUInt16LE(files.length, 10)
  end.writeUInt32LE(directory.length, 12)
  end.writeUInt32LE(offset, 16)
  return new Uint8Array(Buffer.concat([...local, directory, end]))
}

test('reads exact basenames from stored and deflated entries', () => {
  const files = readZipBasenames(zip([['parent/a.json', 'stored', 0], ['b.out', 'compressed', 8]]))
  expect(new TextDecoder().decode(files.get('a.json'))).toBe('stored')
  expect(new TextDecoder().decode(files.get('b.out'))).toBe('compressed')
})

test.each(['signed', 'unsigned'] as const)('reads %s data descriptors', (descriptor) => {
  expect(new TextDecoder().decode(readZipBasenames(zip([['a', 'value', 8]], descriptor)).get('a'))).toBe('value')
})

test('rejects corrupt data descriptors', () => {
  const value = Buffer.from(zip([['a', 'value', 8]], 'signed'))
  value.writeUInt32LE(99, 30 + 1 + 5 + 8)
  expect(() => readZipBasenames(value)).toThrow('descriptor')
})

test('rejects overlapping local entries', () => {
  const value = Buffer.from(zip([['a', 'one', 0], ['b', 'two', 0]]))
  const directory = value.readUInt32LE(value.length - 6)
  value.writeUInt32LE(0, directory + 46 + 1 + 42)
  expect(() => readZipBasenames(value)).toThrow('contiguous')
})

test('rejects malformed archives and duplicate basenames', () => {
  expect(() => readZipBasenames(new Uint8Array([1, 2, 3]))).toThrow(InvalidZip)
  expect(() => readZipBasenames(zip([['a/x', 'one', 0], ['b/x', 'two', 0]]))).toThrow('basename')
})

test('accepts the current planner artifact file count bound', () => {
  const files = Array.from({ length: 32 }, (_, index) => [`path/${index}.txt`, String(index), 0] as [string, string, 0])
  expect(readZipBasenames(zip(files)).size).toBe(32)
})
