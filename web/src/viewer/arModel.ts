// AR で見るときに、メッシュ（SfM の座標）を現実の机や床の上に立てて置くための変換（画面に依存しない計算）。
//
// SfM の座標は原点・向き・縮尺が任意なので、scene.json の手掛かりで次のように直す:
//   向き    scene.json の up（真上）を AR の +y に合わせる
//   正面    最初に撮った写真の側を +z（置いたときに利用者の方を向ける）
//   原点    被写体の中心（center）の真下、メッシュの底（下から 2% の高さ）を y = 0 にする
//   大きさ  metersPerUnit があれば実物大。無ければ半径（radius）が defaultRadiusMeters になる大きさ（目安）
import * as THREE from 'three';
import type { SceneInfo } from '../api/types';

/** 実際の大きさが分からないときの、表示の半径（メートル）。机の上に置いて見やすい大きさ。 */
export const defaultRadiusMeters = 0.15;

export interface ModelPlacement {
  /** モデルの座標 → 置いた点を原点とする座標（y が上、メートル）への変換。 */
  matrix: THREE.Matrix4;
  /** 実物大か（scene.json に metersPerUnit があった）。 */
  realScale: boolean;
}

/**
 * @param info     scene.json
 * @param vertices メッシュの頂点（モデルの座標）。底の高さを決めるのに使う（間引いたものでよい）
 */
export function placeModel(info: SceneInfo, vertices: readonly THREE.Vector3[]): ModelPlacement {
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

  const realScale = !!info.metersPerUnit && info.metersPerUnit > 0;
  const radius = info.radius > 0 ? info.radius : 1;
  const scale = realScale ? info.metersPerUnit! : defaultRadiusMeters / radius;

  // 底: 中心から水平に 1.5 × radius 以内の頂点の高さの、下から 2%。
  // 遠くの床や背景まで入れると、それに引っ張られて対象が宙に浮くので、近くだけを見る。
  const heights: number[] = [];
  const v = new THREE.Vector3();
  for (const p of vertices) {
    v.copy(p).sub(center).applyQuaternion(rotation);
    if (Math.hypot(v.x, v.z) <= 1.5 * radius) heights.push(v.y);
  }
  heights.sort((a, b) => a - b);
  const base = heights.length > 0 ? heights[Math.floor(heights.length * 0.02)]! : -radius;

  // y' = scale · (R · (p − center)).y − scale · base
  const matrix = new THREE.Matrix4()
    .makeTranslation(0, -scale * base, 0)
    .multiply(new THREE.Matrix4().makeScale(scale, scale, scale))
    .multiply(new THREE.Matrix4().makeRotationFromQuaternion(rotation))
    .multiply(new THREE.Matrix4().makeTranslation(-center.x, -center.y, -center.z));
  return { matrix, realScale };
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
