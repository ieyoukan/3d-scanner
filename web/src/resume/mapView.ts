// 位置合わせ（VPS）の結果を「真上から見た地図」に描くための計算。画面に依存しない純粋な関数だけを置く。
//
// サーバーが返す位置と向きは、SfM（COLMAP）の世界座標のまま。原点も向きも「たまたまそうなった」もので、
// どちらが上かも分からない。そこで scene.json の手掛かりを使う:
//   up      写真を撮ったときのカメラの「上」の平均（≒ 重力の逆向き）
//   center  被写体の中心
//   cameraPositions  前回、写真を撮った位置
//
// 地図の平面は up に垂直な平面（＝床と平行）。その平面の中で、画面の「上」を
//   前回の最初の写真の位置 → 被写体の中心
// の向きにする。前回の撮り始めの位置が中心の下に来て、「そこから被写体を見ている」向きの地図になる。
import type { SceneInfo } from '../api/types';

export type Vec3 = [number, number, number];
/** 地図上の点。x は右、y は下（SVG と同じ向き）。 */
export type MapPoint = [number, number];

export interface MapFrame {
  origin: Vec3;
  /** 画面の右に当たる世界座標の向き。 */
  right: Vec3;
  /** 画面の上（奥）に当たる世界座標の向き。 */
  ahead: Vec3;
}

const dot = (a: Vec3, b: Vec3) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
const sub = (a: Vec3, b: Vec3): Vec3 => [a[0] - b[0], a[1] - b[1], a[2] - b[2]];
const scale = (a: Vec3, k: number): Vec3 => [a[0] * k, a[1] * k, a[2] * k];
const cross = (a: Vec3, b: Vec3): Vec3 => [
  a[1] * b[2] - a[2] * b[1],
  a[2] * b[0] - a[0] * b[2],
  a[0] * b[1] - a[1] * b[0],
];

function normalize(v: Vec3): Vec3 | null {
  const n = Math.hypot(v[0], v[1], v[2]);
  if (!Number.isFinite(n) || n < 1e-9) return null;
  return [v[0] / n, v[1] / n, v[2] / n];
}

/** v から up 方向の成分を取り除く（床と平行な成分だけにする）。 */
function flatten(v: Vec3, up: Vec3): Vec3 {
  return sub(v, scale(up, dot(v, up)));
}

/** scene.json から地図の向きを決める。 */
export function buildMapFrame(info: Pick<SceneInfo, 'up' | 'center' | 'cameraPositions'>): MapFrame {
  const up = normalize(info.up) ?? [0, -1, 0];
  // 画面の上: 最初の写真の位置から中心へ向かう向き。求められなければ、up に垂直な適当な向き。
  const first = info.cameraPositions[0];
  let ahead = first ? normalize(flatten(sub(info.center, first), up)) : null;
  if (!ahead) {
    const helper: Vec3 = Math.abs(up[0]) < 0.9 ? [1, 0, 0] : [0, 0, 1];
    ahead = normalize(flatten(helper, up)) ?? [0, 0, 1];
  }
  // 真上（+up）から見下ろしたとき、画面の右 × 画面の上 = 手前（+up）になるように右を決める。
  // (ahead × up) × ahead = up なので、右 = ahead × up。
  const right = normalize(cross(ahead, up)) ?? [1, 0, 0];
  return { origin: info.center, right, ahead };
}

/** 世界座標の点を地図上の点にする。 */
export function toMap(frame: MapFrame, p: Vec3): MapPoint {
  const d = sub(p, frame.origin);
  return [dot(d, frame.right), -dot(d, frame.ahead)];
}

/**
 * カメラの向き（レンズの向き）を地図上の向き（長さ1）にする。
 * ほぼ真下や真上を向いていて、床と平行な成分が小さいときは null（向きの矢印を描かない）。
 */
export function headingOnMap(frame: MapFrame, forward: Vec3): MapPoint | null {
  const x = dot(forward, frame.right);
  const y = -dot(forward, frame.ahead);
  const n = Math.hypot(x, y);
  if (!Number.isFinite(n) || n < 0.2) return null;
  return [x / n, y / n];
}

export interface MapView {
  /** SVG の viewBox（正方形）。 */
  minX: number;
  minY: number;
  size: number;
}

/** すべての点が収まる正方形の表示範囲を、周りに余白を付けて求める。 */
export function fitView(points: MapPoint[], minSize: number, margin = 0.15): MapView {
  if (points.length === 0) return { minX: -minSize / 2, minY: -minSize / 2, size: minSize };
  let x0 = Infinity,
    y0 = Infinity,
    x1 = -Infinity,
    y1 = -Infinity;
  for (const [x, y] of points) {
    x0 = Math.min(x0, x);
    y0 = Math.min(y0, y);
    x1 = Math.max(x1, x);
    y1 = Math.max(y1, y);
  }
  const size = Math.max(x1 - x0, y1 - y0, minSize) * (1 + 2 * margin);
  const cx = (x0 + x1) / 2;
  const cy = (y0 + y1) / 2;
  return { minX: cx - size / 2, minY: cy - size / 2, size };
}
