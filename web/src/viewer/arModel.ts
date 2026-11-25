// AR で見るときに、メッシュ（SfM の座標）を現実の机や床の上に立てて置くための変換（画面に依存しない計算）。
//
// SfM の座標は原点・向き・縮尺が任意なので、scene.json の手掛かりで次のように直す:
//   向き    scene.json の up（真上）を AR の +y に合わせる
//   正面    最初に撮った写真の側を +z（置いたときに利用者の方を向ける）
//   原点    被写体の中心（center）の真下、メッシュの底（下から 2% の高さ）を y = 0 にする
//   切り抜き 物を囲んで撮ったときは、中心から「撮ったカメラまでの距離（中央値）× keepFraction」より内側の面だけ残す
//   大きさ  metersPerUnit があれば実物大。無ければ「撮ったときにカメラが assumedCameraMeters 先にいた」とした大きさ（目安）
//
// なぜ切り抜くのか: 簡易メッシュ（疎な点群のドロネー分割）は、背景の壁や床の点まで面でつなぐ。1周の動画 2 本で測ると、
// 頂点の 81% と 9% がカメラより外（背景）にあり、最も遠い頂点はカメラまでの距離の 8 倍・9 倍だった。そのまま AR に
// 置くと、背景の大きな膜がスマホのすぐ近くや周りに広がり、少し動いただけで大きな面が画面を高速で横切る
// （「すごい速さで動いてどこかへ行く」）。物はカメラの輪の内側にあるので、そこだけ残せばよい。
// 大きさも、背景込みの radius（点の 75% が入る半径）ではなく、カメラまでの距離で決める（radius は背景に引っ張られる）。
//
// 風景モード（歩きながら空間を撮った）は囲むカメラの輪が無いので切り抜かず、全体を模型の大きさにする。
import * as THREE from 'three';
import type { SceneInfo } from '../api/types';

/** 物の切り抜き: 撮ったカメラまでの距離（中央値）に対する割合。 */
export const keepFraction = 0.6;
/** 実際の大きさが分からないとき、撮ったカメラまでの距離をこの長さ（メートル）とみなす。 */
export const assumedCameraMeters = 0.6;
/** 風景モードで実際の大きさが分からないとき、頂点の 90% が入る半径をこの長さ（メートル）にする（机の上の模型）。 */
export const sceneModelRadiusMeters = 0.3;

export interface ModelPlacement {
  /** モデルの座標 → 置いた点を原点とする座標（y が上、メートル）への変換。 */
  matrix: THREE.Matrix4;
  /** 実物大か（scene.json に metersPerUnit があった）。 */
  realScale: boolean;
  /** 残す範囲: 中心からこの距離（モデルの座標）より内側の面だけ残す。null なら切り抜かない。 */
  keepWithin: number | null;
}

/**
 * @param info     scene.json
 * @param vertices メッシュの頂点（モデルの座標）。底の高さを決めるのに使う（間引いたものでよい）
 * @param scene    風景モードで撮ったスキャンか
 */
export function placeModel(info: SceneInfo, vertices: readonly THREE.Vector3[], scene = false): ModelPlacement {
  const center = new THREE.Vector3(...info.center);
  const up = new THREE.Vector3(...info.up);
  if (up.lengthSq() < 1e-12) up.set(0, 1, 0);
  up.normalize();
  const tilt = new THREE.Quaternion().setFromUnitVectors(up, new THREE.Vector3(0, 1, 0));

  // 最初に撮った写真の側を +z へ。
  const rotation = tilt.clone();
  const firstCamera = info.cameraPositions[0];
  if (firstCamera) {
    const d = new THREE.Vector3(...firstCamera).sub(center).applyQuaternion(tilt);
    if (Math.hypot(d.x, d.z) > 1e-9) {
      const yaw = new THREE.Quaternion().setFromAxisAngle(new THREE.Vector3(0, 1, 0), -Math.atan2(d.x, d.z));
      rotation.premultiply(yaw);
    }
  }

  const radius = info.radius > 0 ? info.radius : 1;
  const orbit = cameraDistance(info, center) ?? radius;
  const keepWithin = scene ? null : keepFraction * orbit;
  const kept = keepWithin === null ? vertices : vertices.filter((p) => p.distanceTo(center) <= keepWithin);

  const realScale = !!info.metersPerUnit && info.metersPerUnit > 0;
  let scale: number;
  if (realScale) {
    scale = info.metersPerUnit!;
  } else if (scene) {
    const ds = vertices.map((p) => p.distanceTo(center)).sort((a, b) => a - b);
    const r90 = ds.length > 0 ? ds[Math.floor(ds.length * 0.9)]! : radius;
    scale = sceneModelRadiusMeters / (r90 > 0 ? r90 : radius);
  } else {
    scale = assumedCameraMeters / orbit;
  }

  // 底: 残す範囲の頂点の高さの、下から 2%。
  const heights: number[] = [];
  const v = new THREE.Vector3();
  for (const p of kept) {
    v.copy(p).sub(center).applyQuaternion(rotation);
    heights.push(v.y);
  }
  heights.sort((a, b) => a - b);
  const base = heights.length > 0 ? heights[Math.floor(heights.length * 0.02)]! : -radius;

  // y' = scale · (R · (p − center)).y − scale · base
  const matrix = new THREE.Matrix4()
    .makeTranslation(0, -scale * base, 0)
    .multiply(new THREE.Matrix4().makeScale(scale, scale, scale))
    .multiply(new THREE.Matrix4().makeRotationFromQuaternion(rotation))
    .multiply(new THREE.Matrix4().makeTranslation(-center.x, -center.y, -center.z));
  return { matrix, realScale, keepWithin };
}

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

/** 頂点を最大 max 個まで等間隔に取り出す（モデルの座標。各メッシュの変換を掛けたもの）。 */
export function sampleVertices(root: THREE.Object3D, max = 20000): THREE.Vector3[] {
  root.updateMatrixWorld(true);
  const meshes: THREE.Mesh[] = [];
  let total = 0;
  root.traverse((obj) => {
    const mesh = obj as THREE.Mesh;
    const pos = mesh.isMesh ? mesh.geometry.attributes.position : undefined;
    if (pos) {
      meshes.push(mesh);
      total += pos.count;
    }
  });
  const step = Math.max(1, Math.floor(total / max));
  const out: THREE.Vector3[] = [];
  for (const mesh of meshes) {
    const pos = mesh.geometry.attributes.position!;
    for (let i = 0; i < pos.count; i += step) {
      out.push(new THREE.Vector3().fromBufferAttribute(pos, i).applyMatrix4(mesh.matrixWorld));
    }
  }
  return out;
}
