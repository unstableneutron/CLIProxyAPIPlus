import { expect, test } from "bun:test";
import { deflateRawSync } from "node:zlib";
import { InvalidZip, readZipBasenames } from "./zip";

function crc32(v: Uint8Array) { let c=0xffffffff; for(const x of v){c^=x; for(let i=0;i<8;i++)c=(c>>>1)^(0xedb88320&-(c&1));} return (c^0xffffffff)>>>0; }
function zip(files: Array<[string,string,0|8]>, descriptor: "none"|"signed"|"unsigned"="none"): Uint8Array {
  const local:Buffer[]=[], central:Buffer[]=[]; let offset=0;
  for(const [name,text,method] of files){const n=Buffer.from(name),raw=Buffer.from(text),data=method?deflateRawSync(raw):raw,crc=crc32(raw),flags=descriptor==="none"?0:8,l=Buffer.alloc(30+n.length);l.writeUInt32LE(0x04034b50);l.writeUInt16LE(20,4);l.writeUInt16LE(flags,6);l.writeUInt16LE(method,8);if(!flags){l.writeUInt32LE(crc,14);l.writeUInt32LE(data.length,18);l.writeUInt32LE(raw.length,22);}l.writeUInt16LE(n.length,26);n.copy(l,30);const d=Buffer.alloc(descriptor==="signed"?16:descriptor==="unsigned"?12:0);let q=0;if(descriptor==="signed"){d.writeUInt32LE(0x08074b50);q=4;}if(flags){d.writeUInt32LE(crc,q);d.writeUInt32LE(data.length,q+4);d.writeUInt32LE(raw.length,q+8);}local.push(l,data,d);const c=Buffer.alloc(46+n.length);c.writeUInt32LE(0x02014b50);c.writeUInt16LE(20,4);c.writeUInt16LE(20,6);c.writeUInt16LE(flags,8);c.writeUInt16LE(method,10);c.writeUInt32LE(crc,16);c.writeUInt32LE(data.length,20);c.writeUInt32LE(raw.length,24);c.writeUInt16LE(n.length,28);c.writeUInt32LE(offset,42);n.copy(c,46);central.push(c);offset+=l.length+data.length+d.length;}const directory=Buffer.concat(central),e=Buffer.alloc(22);e.writeUInt32LE(0x06054b50);e.writeUInt16LE(files.length,8);e.writeUInt16LE(files.length,10);e.writeUInt32LE(directory.length,12);e.writeUInt32LE(offset,16);return new Uint8Array(Buffer.concat([...local,directory,e]));
}

test("reads exact basenames from stored and deflated entries",()=>{const files=readZipBasenames(zip([["parent/a.json","stored",0],["b.out","compressed",8]]));expect(new TextDecoder().decode(files.get("a.json"))).toBe("stored");expect(new TextDecoder().decode(files.get("b.out"))).toBe("compressed");});
test("reads signed data descriptors for stored and deflated entries",()=>expect([...readZipBasenames(zip([["a","stored",0],["b","deflated",8]],"signed")).keys()]).toEqual(["a","b"]));
test("reads signatureless data descriptors",()=>expect(new TextDecoder().decode(readZipBasenames(zip([["a","value",8]],"unsigned")).get("a"))).toBe("value"));
test("rejects corrupt data descriptors",()=>{const value=Buffer.from(zip([["a","value",8]],"signed"));value.writeUInt32LE(99,30+1+5+8);expect(()=>readZipBasenames(value)).toThrow("descriptor");});
test.each(["signed","unsigned"] as const)("rejects contradictory %s descriptor local fields",(descriptor)=>{
  for(const offset of [14,18,22]) {
    const value=Buffer.from(zip([["a","value",8]],descriptor));
    value.writeUInt32LE(1,offset);
    expect(()=>readZipBasenames(value)).toThrow("descriptor metadata");
  }
});
test("rejects overlapping or aliased local entries",()=>{const value=Buffer.from(zip([["a","one",0],["b","two",0]]));const directory=value.readUInt32LE(value.length-6);value.writeUInt32LE(0,directory+46+1+42);expect(()=>readZipBasenames(value)).toThrow("contiguous");});
test("rejects unsupported flags",()=>{const value=Buffer.from(zip([["a","one",0]]));const directory=value.readUInt32LE(value.length-6);value.writeUInt16LE(1,directory+8);value.writeUInt16LE(1,6);expect(()=>readZipBasenames(value)).toThrow("unsupported");});
test("rejects malformed archives",()=>expect(()=>readZipBasenames(new Uint8Array([1,2,3]))).toThrow(InvalidZip));
test("rejects duplicate basenames",()=>expect(()=>readZipBasenames(zip([["a/x","one",0],["b/x","two",0]]))).toThrow("basename"));
test("rejects oversized declared output",()=>{const value=Buffer.from(zip([["x","x",0]]));const directory=value.readUInt32LE(value.length-6);value.writeUInt32LE(1_000_001,directory+24);expect(()=>readZipBasenames(value)).toThrow("unsupported");});
