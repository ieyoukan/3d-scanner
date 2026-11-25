// 「続きを撮る」を AR で: 位置合わせ（VPS）の結果から、地図（前回の SfM の座標）を AR の座標に重ねる変換を求める
// （画面に依存しない計算）。
//
// 位置合わせ1回で分かること（fix）:
//   地図での カメラの位置 c とレンズの向き g        ← サーバーの VPS
//   AR での   カメラの位置 a とレンズの向き h        ← 同じ写真を撮った瞬間の AR の姿勢
// 求めたいのは  AR = s · R · 地図 + t。
//
//   R  まず地図の「真上」（scene.json の up）を AR の +y に合わせ（傾き）、次に鉛直の軸のまわりに回して
//      g と h の水平の向きをそろえる（方位）。レンズの向きと真上しか使わないので、写真の上下（カメラの軸の
//      ねじれ）に左右されない。位置合わせが複数あれば、方位は平均する。
//   s  scene.json に metersPerUnit（AR で撮ったスキャンの実際の大きさ）があればそれ。無ければ、離れた
//      2か所以上の位置合わせから、AR で動いた距離と地図で動いた距離の比で求める（最小二乗）。
//   t  a の平均 − s · R · c の平均。
import * as THREE from 'three';

export type Vec3 = readonly [number, number, number];

export interface ArFix {
  /** AR でのカメラの位置（メートル、y が上）とレンズの向き。 */
  arPos: Vec3;
  arFwd: Vec3;
  /** 地図でのカメラの位置とレンズの向き。 */
  mapPos: Vec3;
  mapFwd: Vec3;
}

/** 大きさが分からないとき、位置合わせした場所どうしが AR でこれだけ離れていれば大きさを求める（メートル）。 */
export const minBaselineMeters = 0.3;

export type AlignResult =
  | { kind: 'aligned'; matrix: THREE.Matrix4; scale: number }
  /** 大きさを求めるには、もう1か所（minBaselineMeters 以上離れた所）で位置合わせが要る。 */
  | { kind: 'need-baseline' }
  | { kind: 'none' };

/**
 * @param fixes          位置合わせの結果（新しいものが後ろ）
 * @param up             地図の真上（scene.json の up）
 * @param metersPerUnit  地図の 1 が何メートルか（分かっていれば）
 */
export function solveAlignment(fixes: readonly ArFix[], up: Vec3, metersPerUnit?: number): AlignResult {
  if (fixes.length === 0) return { kind: 'none' };
  const upV = new THREE.Vector3(...up);
  if (upV.lengthSq() < 1e-12) upV.set(0, 1, 0);
  const tilt = new THREE.Quaternion().setFromUnitVectors(upV.normalize(), new THREE.Vector3(0, 1, 0));

  // 方位: 各 fix の「AR での水平の向き − 地図（傾きを直した後）での水平の向き」の平均（円周上の平均）。
  // ほぼ真下を向いて撮ったものは水平の向きがあいまいなので使わない。
  let sx = 0;
  let sy = 0;
  for (const f of fixes) {
    const g = new THREE.Vector3(...f.mapFwd).applyQuaternion(tilt);
    const hLen = Math.hypot(f.arFwd[0], f.arFwd[2]);
    const gLen = Math.hypot(g.x, g.z);
    if (hLen < 0.3 * Math.hypot(...f.arFwd) || gLen < 0.3 * g.length()) continue;
    const yaw = Math.atan2(f.arFwd[0], f.arFwd[2]) - Math.atan2(g.x, g.z);
    sx += Math.cos(yaw);
    sy += Math.sin(yaw);
  }
  if (sx === 0 && sy === 0) return { kind: 'none' };
  const yaw = Math.atan2(sy, sx);
  const rotation = new THREE.Quaternion().setFromAxisAngle(new THREE.Vector3(0, 1, 0), yaw).multiply(tilt);

  const a = fixes.map((f) => new THREE.Vector3(...f.arPos));
  const c = fixes.map((f) => new THREE.Vector3(...f.mapPos).applyQuaternion(rotation));
  const aMean = mean(a);
  const cMean = mean(c);

  let scale: number;
  if (metersPerUnit && metersPerUnit > 0) {
    scale = metersPerUnit;
  } else {
    // AR で十分離れた所どうしがなければ、大きさは決められない。
    const baseline = Math.max(...a.map((p) => p.distanceTo(aMean))) * 2;
    if (fixes.length < 2 || baseline < minBaselineMeters) return { kind: 'need-baseline' };
    let num = 0;
    let den = 0;
    a.forEach((p, i) => {
      const ap = p.clone().sub(aMean);
      const cp = c[i]!.clone().sub(cMean);
      num += ap.dot(cp);
      den += cp.lengthSq();
    });
    if (!(den > 0) || !(num > 0)) return { kind: 'need-baseline' };
    scale = num / den;
  }

  const t = aMean.clone().sub(cMean.clone().multiplyScalar(scale));
  const matrix = new THREE.Matrix4().compose(t, rotation, new THREE.Vector3(scale, scale, scale));
  return { kind: 'aligned', matrix, scale };
}

function mean(points: readonly THREE.Vector3[]): THREE.Vector3 {
  const m = new THREE.Vector3();
  for (const p of points) m.add(p);
  return m.multiplyScalar(1 / points.length);
}
