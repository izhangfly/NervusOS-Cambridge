// Garment outlines and the coarse simulation mesh. No three.js here: the physics worker builds scenes from
// this, and cloth.js (the rendered surface) imports the same outlines.

// Garment outlines in garment coordinates: u across (0..1), v from hem (0) to collar/waist (1).
export const SHAPES = {
  tshirt(u, v) {
    const ax = Math.abs(u - .5);
    if (((u - .5) / .12) ** 2 + ((v - .97) / .12) ** 2 < 1) return false;            // neckline
    const shoulder = .86 - .32 * Math.max(0, ax - .1);
    if (ax <= .27) return v >= 0 && v <= shoulder;                                    // body
    const dx = ax - .27; if (dx > .2) return false;                                   // short sleeves
    const top = .86 - .32 * .17 - .55 * dx, bottom = .63 - .22 * dx;
    return v <= top && v >= bottom;
  },
  jacket(u, v) {
    const ax = Math.abs(u - .5);
    if (v > .74 && ax < (v - .74) * .55 + .015) return false;                         // V-neck / open front
    const shoulder = .88 - .3 * Math.max(0, ax - .12);
    if (ax <= .26) return v >= 0 && v <= shoulder;
    const dx = ax - .26; if (dx > .235) return false;                                  // long sleeves
    const top = .88 - .3 * .14 - .95 * dx, bottom = .64 - .95 * dx;
    return v <= top && v >= bottom;
  },
  trousers(u, v) {
    const ax = Math.abs(u - .5);
    if (v > .985) return false;
    if (v >= .6) return ax <= .34;                                                    // waist and seat
    const gap = .018 + .05 * (.6 - v) / .6, outer = .34 + .035 * (.6 - v) / .6;     // crotch opens, legs flare
    return ax >= gap && ax <= outer;
  },
};

// Coarse simulation mesh: grid points inside (or within half a cell of) the outline.
export function garmentMesh(kind, position, size = .13, N = 5) {
  const shape = SHAPES[kind] || SHAPES.tshirt, H = size * 1.12;
  const near = (u, v) => { const h = .5 / N; for (const [a, b] of [[0, 0], [h, 0], [-h, 0], [0, h], [0, -h], [h, h], [-h, h], [h, -h], [-h, -h]]) if (shape(u + a, v + b)) return true; return false; };
  const points = [], uv = [], indices = [], index = new Int32Array((N + 1) * (N + 1)).fill(-1);
  for (let j = 0; j <= N; j++) for (let i = 0; i <= N; i++) {
    const u = i / N, v = j / N; if (!near(u, v)) continue;
    index[j * (N + 1) + i] = points.length; uv.push(u, v);
    points.push([position[0] + (u - .5) * size, position[1] + (v - .5) * H, position[2] + .0012 * Math.sin(i * 1.7 + j * .9)]);
  }
  const at = (i, j) => (i < 0 || j < 0 || i > N || j > N) ? -1 : index[j * (N + 1) + i];
  for (let j = 0; j < N; j++) for (let i = 0; i < N; i++) {
    const a = at(i, j), b = at(i + 1, j), c = at(i, j + 1), d = at(i + 1, j + 1);
    if (a >= 0 && b >= 0 && c >= 0) indices.push(a, b, c);
    if (b >= 0 && c >= 0 && d >= 0) indices.push(b, d, c);
  }
  return { points, indices, uv, grid: { N, index: Array.from(index), size, H, kind } };
}

// Clip a fine triangle grid to the garment silhouette. Shared edge intersections remove the staircase
// around sleeves, collars and trouser legs without adding physics vertices.
export function garmentOutline(kind, M=40){
 const shape=SHAPES[kind]||SHAPES.tshirt,uv=[],indices=[],vertices=new Map(),crossings=new Map();
 const point=id=>[id%(M+1)/M,Math.floor(id/(M+1))/M],inside=id=>shape(...point(id));
 const vertex=(key,p)=>{if(!vertices.has(key)){vertices.set(key,uv.length/2);uv.push(...p)}return vertices.get(key)};
 const crossing=(a,b)=>{
  const lo=Math.min(a,b),hi=Math.max(a,b),key=lo+':'+hi;
  if(!crossings.has(key)){
   const A=point(lo),B=point(hi),at=t=>[A[0]+(B[0]-A[0])*t,A[1]+(B[1]-A[1])*t],inA=inside(lo);let l=0,h=1;
   for(let i=0;i<22;i++){const t=(l+h)/2;if(shape(...at(t))===inA)l=t;else h=t}
   crossings.set(key,vertex('edge:'+key,at((l+h)/2)));
  }return crossings.get(key);
 };
 const triangle=ids=>{
  const clipped=[];
  for(let i=0;i<3;i++){const a=ids[i],b=ids[(i+1)%3],inA=inside(a);if(inA)clipped.push(vertex('grid:'+a,point(a)));if(inA!==inside(b))clipped.push(crossing(a,b))}
  for(let i=1;i+1<clipped.length;i++){
   const [a,b,c]=[clipped[0],clipped[i],clipped[i+1]],area=(uv[2*b]-uv[2*a])*(uv[2*c+1]-uv[2*a+1])-(uv[2*b+1]-uv[2*a+1])*(uv[2*c]-uv[2*a]);
   if(area>1e-10)indices.push(a,b,c);
  }
 };
 for(let j=0;j<M;j++)for(let i=0;i<M;i++){const a=j*(M+1)+i,b=a+1,c=a+M+1,d=c+1;triangle([a,b,c]);triangle([b,d,c])}
 return {uv,indices};
}
