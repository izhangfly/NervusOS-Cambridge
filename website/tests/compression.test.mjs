import test from'node:test';import assert from'node:assert/strict';
import{mkdtemp,mkdir,writeFile,readFile,readdir,rm}from'node:fs/promises';
import{execFile}from'node:child_process';import{promisify}from'node:util';
import{gunzipSync,brotliDecompressSync}from'node:zlib';import{tmpdir}from'node:os';
import path from'node:path';import{fileURLToPath}from'node:url';
test('compression targets the final published directory and round-trips model bundles and darkened CSS',async()=>{
 const root=await mkdtemp(path.join(tmpdir(),'nervus published ')),folder=path.join(root,'assets');
 try{
  await mkdir(folder);const files={'visual.bin':Buffer.from(Array.from({length:4096},(_,i)=>i%251)),'demo.css':Buffer.from('.panel{background:#0e0e0d;color:#e5ecec}'.repeat(40))};
  for(const[name,bytes]of Object.entries(files))await writeFile(path.join(folder,name),bytes);
  const run=()=>promisify(execFile)(process.execPath,[fileURLToPath(new URL('../server/compress.mjs',import.meta.url)),root]);
  await run();
  for(const[name,bytes]of Object.entries(files)){
   assert.deepEqual(gunzipSync(await readFile(path.join(folder,name+'.gz'))),bytes);
   assert.deepEqual(brotliDecompressSync(await readFile(path.join(folder,name+'.br'))),bytes);
  }
  await run();assert.equal((await readdir(folder)).length,6,'repeat compression must not compress sidecars again');
 }finally{await rm(root,{recursive:true,force:true})}
});
