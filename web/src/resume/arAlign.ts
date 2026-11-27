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

/**
 * 写真 1 枚の位置合わせから、地図の座標 → AR の座標の変換を作る（「AR で見る」の実物に重ねる）。
 *
 * solveAlignment と違い、AR の「位置」を使わない。AR の位置（移動）は端末や場所によって大きく狂う
 * （実機の記録で、40 秒の撮影で 18m 以上飛んでいた）が、向き（回転）は正確だった（写真から求めた向きと 1.5° 以内）。
 * そこで、写真を撮った瞬間の AR のカメラの向きと、位置合わせで分かった地図でのカメラの向き（forward と down）を
 * そろえて回転を決め、カメラの位置どうしを重ねる。こうすると、その瞬間のカメラから見た地図は写真とぴったり重なる。
 * その場で見回す分には、AR の向きだけで正しく追える（位置の狂いは「重ねた瞬間のカメラの位置」に固定して無視する）。
 *
 * 縮尺 scale（地図の 1 が何メートルか）は、カメラの位置から見る限り見た目に影響しない（全体を同じ倍率で
 * 大きくしても、カメラの位置を中心にすれば同じに見える）。実物大が分かっていればそれ、無ければ見やすい値を渡す。
 *
 * @param map   地図でのカメラの位置・レンズの向き・画像の下方向
 * @param ar    同じ瞬間の AR のカメラの位置と向き（四元数 x, y, z, w。WebXR の約束: x 右・y 上・-z が前）
 */
export function snapshotAlignment(
  map: { position: Vec3; forward: Vec3; down: Vec3 },
  ar: { position: Vec3; orientation: Quat },
  scale: number,
): THREE.Matrix4 {
  return poseMatrix(snapshotPose(map, ar.orientation), scale, ar.position);
}

/** 四元数（x, y, z, w）。 */
export type Quat = readonly [number, number, number, number];

/**
 * 写真 1 枚の位置合わせで決まる重ね方のうち、AR の位置に依らない部分。
 * 「地図を、地図でのカメラの位置 camera を中心に rotation だけ回し、そこを AR のカメラの位置に置く」。
 */
export interface SnapshotPose {
  /** 地図 → AR の回転。 */
  rotation: THREE.Quaternion;
  /** 地図でのカメラの位置。 */
  camera: THREE.Vector3;
}

/** 位置合わせの結果（地図でのカメラ）と、同じ瞬間の AR のカメラの向きから、重ね方を求める（snapshotAlignment の説明を参照）。 */
export function snapshotPose(map: { position: Vec3; forward: Vec3; down: Vec3 }, arOrientation: Quat): SnapshotPose {
  // 地図でのカメラの軸（COLMAP の約束: x 右・y 下・z 前）。右 = 下 × 前。
  const fwd = new THREE.Vector3(...map.forward).normalize();
  const down = new THREE.Vector3(...map.down).normalize();
  const right = new THREE.Vector3().crossVectors(down, fwd).normalize();
  // 下と前が少しずれていても直交するよう、下を作り直す。
  down.crossVectors(fwd, right).normalize();
  const camToMap = new THREE.Matrix4().makeBasis(right, down, fwd);
  // AR のカメラの軸を COLMAP の約束に直す（y と z の向きが逆）。
  const flip = new THREE.Matrix4().makeScale(1, -1, -1);
  const arCam = new THREE.Matrix4().makeRotationFromQuaternion(new THREE.Quaternion(...arOrientation));
  // R = (AR のカメラ → AR) · (COLMAP の軸 → XR の軸) · (地図 → カメラ)
  const rotation = arCam.multiply(flip).multiply(camToMap.transpose());
  return {
    rotation: new THREE.Quaternion().setFromRotationMatrix(rotation),
    camera: new THREE.Vector3(...map.position),
  };
}

/**
 * 重ね方を、地図の座標 → AR の座標の変換にする。地図でのカメラの位置が、AR の arPosition（省略すると原点）に来る。
 * 「AR で見る」は AR の位置を使わないので、原点に置いた変換を、毎フレーム AR のカメラの位置へ動かして使う。
 */
export function poseMatrix(pose: SnapshotPose, scale: number, arPosition: Vec3 = [0, 0, 0], out = new THREE.Matrix4()): THREE.Matrix4 {
  const c = pose.camera.clone().applyQuaternion(pose.rotation).multiplyScalar(scale);
  const t = new THREE.Vector3(...arPosition).sub(c);
  return out.compose(t, pose.rotation, new THREE.Vector3(scale, scale, scale));
}

/**
 * 2 つの重ね方の途中（k = 0 で a、1 で b）。回転は球面補間、カメラの位置は直線補間。
 * カメラの位置を中心に回るので、合わせ直したときの動きが「その場で首を振った」ように見える。
 */
export function blendPose(a: SnapshotPose, b: SnapshotPose, k: number): SnapshotPose {
  return {
    rotation: a.rotation.clone().slerp(b.rotation, k),
    camera: a.camera.clone().lerp(b.camera, k),
  };
}

/** 2 つの重ね方の回転の違い（度）。 */
export function poseTurnDegrees(a: SnapshotPose, b: SnapshotPose): number {
  return THREE.MathUtils.radToDeg(a.rotation.angleTo(b.rotation));
}

/**
 * いまの AR のカメラのレンズの向きを、地図の向きに直す（位置合わせの手がかりにする）。
 * 重ね方の回転は地図 → AR なので、その逆で戻す。AR のカメラは -z が前。
 */
export function mapForward(pose: SnapshotPose, arOrientation: Quat): Vec3 {
  const f = new THREE.Vector3(0, 0, -1)
    .applyQuaternion(new THREE.Quaternion(...arOrientation))
    .applyQuaternion(pose.rotation.clone().invert())
    .normalize();
  return [f.x, f.y, f.z];
}
