"""Weld and reduce PiPER/Gen3 display meshes. Explicit inertias/contact shapes stay unchanged."""
from pathlib import Path
import json,shutil,xml.etree.ElementTree as E
import trimesh
root=Path(__file__).resolve().parents[1]
for arm in ['piper','gen3']:
 folder=root/'public/models'/arm; cache=root/'build/model-sources'/arm;cache.mkdir(parents=True,exist_ok=True)
 tree=E.parse(folder/(arm+'.xml')); provenance=json.loads((folder/'provenance.json').read_text())
 old={m['browser']:m['source'] for m in provenance.get('web_meshes',[])}
 changed=[];before=after=0
 for node in tree.findall('.//asset/mesh'):
  previous=node.get('file');name=old.get(previous,previous);source=cache/name
  if not source.exists():shutil.copy2(folder/'assets'/name,source)
  mesh=trimesh.load(source,force='mesh',process=False);mesh.merge_vertices(merge_tex=True,merge_norm=True)
  n=len(mesh.faces)
  if n>3000:mesh=mesh.simplify_quadric_decimation(face_count=3000,aggression=5)
  out=Path(name).stem+'_web-v1.stl';mesh.export(folder/'assets'/out,file_type='stl')
  node.set('name',node.get('name',Path(name).stem));node.set('file',out)
  before+=source.stat().st_size;after+=(folder/'assets'/out).stat().st_size
  changed.append({'source':name,'browser':out,'source_faces':n,'browser_faces':len(mesh.faces)})
  if previous!=out:(folder/'assets'/previous).unlink(missing_ok=True)
 E.indent(tree);tree.write(folder/(arm+'.xml'),encoding='unicode')
 provenance['web_meshes']=changed
 note='Welded and reduced display meshes for browser delivery; explicit inertias/contact shapes unchanged'
 if note not in provenance['changes']:provenance['changes'].append(note)
 (folder/'provenance.json').write_text(json.dumps(provenance,indent=2)+'\n')
 print(json.dumps({'arm':arm,'before':before,'after':after}))
