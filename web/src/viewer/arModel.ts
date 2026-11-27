// AR でメッシュを重ねるときの、メッシュの下ごしらえ（画面に依存しない計算）。
//
// 切り抜き: 物を囲んで撮ったときは、中心から「撮ったカメラまでの距離（中央値）× keepFraction」より内側の面だけ残す。
// 簡易メッシュ（疎な点群のドロネー分割）は、背景の壁や床の点まで面でつなぐ。1周の動画 2 本で測ると、頂点の 81% と 9% が
// カメラより外（背景）にあり、最も遠い頂点はカメラまでの距離の 8 倍・9 倍だった。そのまま AR に出すと、背景の大きな膜が
// 画面を覆う。物はカメラの輪の内側にあるので、そこだけ残せばよい。風景モード（歩きながら空間を撮った）は切り抜かない。
import * as THREE from 'three';
import type { SceneInfo } from '../api/types';

/** 物の切り抜き: 撮ったカメラまでの距離（中央値）に対する割合。 */
export const keepFraction = 0.6;

/** 撮ったカメラから中心までの距離の中央値（カメラが無ければ null）。 */
export function cameraDistance(info: SceneInfo, center = new THREE.Vector3(...info.center)): number | null {
  const ds = info.cameraPositions.map((c) => new THREE.Vector3(...c).distanceTo(center)).sort((a, b) => a - b);
  const d = ds[Math.floor(ds.length / 2)];
  return d && d > 0 ? d : null;
}

/**
 * 中心から maxDistance より外に頂点が1つでもある三角形を消す（背景の膜を取り除く）。
 * 頂点そのものは残し、三角形の並び（index）だけを作り直す。座標は root から見たもの（各メッシュの変換を掛ける）。
 */
export function cropMesh(root: THREE.Object3D, center: THREE.Vector3, maxDistance: number): void {
  root.updateMatrixWorld(true);
  const p = new THREE.Vector3();
  root.traverse((obj) => {
    const mesh = obj as THREE.Mesh;
    const pos = mesh.isMesh ? mesh.geometry.attributes.position : undefined;
    if (!pos) return;
    const inside = new Uint8Array(pos.count);
    for (let i = 0; i < pos.count; i++) {
      p.fromBufferAttribute(pos, i).applyMatrix4(mesh.matrixWorld);
      inside[i] = p.distanceTo(center) <= maxDistance ? 1 : 0;
    }
    const index = mesh.geometry.index;
    const count = index ? index.count : pos.count;
    const kept: number[] = [];
    for (let t = 0; t + 2 < count; t += 3) {
      const a = index ? index.getX(t) : t;
      const b = index ? index.getX(t + 1) : t + 1;
      const c = index ? index.getX(t + 2) : t + 2;
      if (inside[a] && inside[b] && inside[c]) kept.push(a, b, c);
    }
    mesh.geometry.setIndex(kept);
  });
}
