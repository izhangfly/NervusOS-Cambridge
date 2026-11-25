// Workers need the joint/contact model, never the display mesh triangles. Every supplied
// body has an explicit inertia; retaining the remaining XML preserves physical dynamics.
export function physicsXML(xml){
 return xml.replace(/<mesh\b[^>]*\/>/g,'').replace(/<geom\b[^>]*type="mesh"[^>]*\/>/g,'').replace(/<geom\b[^>]*mesh="[^"]+"[^>]*\/>/g,'');
}
export function decodeVisualBundle(buffer){
 const length=new DataView(buffer).getUint32(0,true),header=JSON.parse(new TextDecoder().decode(new Uint8Array(buffer,4,length)));
 const meshes={};let offset=4+length;
 for(const file of header.files){meshes[file.name]=new Uint8Array(buffer,offset,file.bytes);offset+=file.bytes}
 if(offset!==buffer.byteLength)throw Error('Invalid robot visual bundle');
 return{id:header.id,xml:header.xml,meshes};
}
