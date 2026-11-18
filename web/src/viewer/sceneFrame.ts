// SfM の座標系を、ビューアで見やすい向き・位置に直すための計算。
//
// 問題:
//   SfM（COLMAP）が出す座標は、原点も向きも縮尺も「たまたまそうなった」ものでしかない。
//   そのまま表示すると、モデルが画面の外にあったり、逆さまだったりする。
//
// 手掛かり（サーバーが scene.json に計算してくれている）:
//   up      写真を撮ったときのカメラの「上」の平均。人はスマホをほぼ水平に持つので、重力の逆向きに近い。
//   center  被写体のおおよその中心（全カメラの視線が集まる点。集まらなければ疎な点群の中央値）。
//   radius  中心からの広がり。
//   cameraPositions  写真を撮った位置。
//
// ここで作るもの（ViewFrame）:
//   モデルに掛ける回転と平行移動。これを掛けると
//     - 被写体の中心が原点に来る
//     - 「上」が +Y を向く（three.js も PlayCanvas も +Y が上）
//   そして、最初のカメラ位置（実際に写真を撮った位置の1つ）。
//
// メッシュ・3DGS・点群の3つのビューアが同じ ViewFrame を使うので、タブを切り替えても向きが揃う。
// ファイルそのもの（GLB / PLY）は書き換えない。ダウンロードしたデータは COLMAP の座標系のままで、
// 互いに、また sparse/ のカメラとも重ね合わせられる。
import type { SceneInfo } from '../api/types';

export type Vec3 = [number, number, number];
/** 四元数 (x, y, z, w)。回転を表す4つの数。 */
export type Quat = [number, number, number, number];

export interface ViewFrame {
  /** モデルに掛ける回転。 */
  rotation: Quat;
  /** 回転の後に足す平行移動。 */
  position: Vec3;
  /** カメラの初期位置（原点を見る）。 */
  cameraPosition: Vec3;
  /** 表示範囲の目安（ズームの限界や描画範囲を決めるのに使う）。 */
  radius: number;
  /** scene.json が無く、仮の値で作ったものか。true ならビューア側で形状から位置を合わせ直す。 */
  approximate: boolean;
}

const length = (v: Vec3): number => Math.hypot(v[0], v[1], v[2]);

const cross = (a: Vec3, b: Vec3): Vec3 => [
  a[1] * b[2] - a[2] * b[1],
  a[2] * b[0] - a[0] * b[2],
  a[0] * b[1] - a[1] * b[0],
];

const dot = (a: Vec3, b: Vec3): number => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];

function normalize(v: Vec3): Vec3 | null {
  const n = length(v);
  if (!Number.isFinite(n) || n < 1e-9) return null;
  return [v[0] / n, v[1] / n, v[2] / n];
}

/**
 * 単位ベクトル from を to に重ねる最短の回転を返す。
 *
 * 2つのベクトルに垂直な軸（外積）のまわりに、2つのなす角だけ回せばよい。
 * 四元数では (軸 × sin(θ/2), cos(θ/2)) と書くが、三角関数を使わずに
 * (from × to, 1 + from·to) を正規化しても同じものが得られる（半角の公式による）。
 */
export function rotationBetween(from: Vec3, to: Vec3): Quat {
  const d = dot(from, to);
  if (d < -0.999999) {
    // ちょうど逆向き。回転軸が1つに決まらないので、from に垂直な軸を適当に選んで 180° 回す。
    const axis = normalize(cross([1, 0, 0], from)) ?? normalize(cross([0, 1, 0], from)) ?? [0, 0, 1];
    return [axis[0], axis[1], axis[2], 0];
  }
  const c = cross(from, to);
  const q: Quat = [c[0], c[1], c[2], 1 + d];
  const n = Math.hypot(q[0], q[1], q[2], q[3]);
  return [q[0] / n, q[1] / n, q[2] / n, q[3] / n];
}

/** ベクトル v を四元数 q で回転する。 */
export function rotate(q: Quat, v: Vec3): Vec3 {
  // v' = v + 2w(u × v) + 2u × (u × v)   （u は q のベクトル部分）
  const u: Vec3 = [q[0], q[1], q[2]];
  const uv = cross(u, v);
  const uuv = cross(u, uv);
  return [
    v[0] + 2 * (q[3] * uv[0] + uuv[0]),
    v[1] + 2 * (q[3] * uv[1] + uuv[1]),
    v[2] + 2 * (q[3] * uv[2] + uuv[2]),
  ];
}

/** COLMAP の座標（Y が下）を Y が上になるよう、X 軸まわりに 180° 回す回転。 */
const flipYDown: Quat = [1, 0, 0, 0];

/** scene.json から ViewFrame を作る。info が無ければ仮の値を返す。 */
export function computeViewFrame(info: SceneInfo | null): ViewFrame {
  if (!info) {
    return { rotation: flipYDown, position: [0, 0, 0], cameraPosition: [0, 0, 5], radius: 1, approximate: true };
  }
  const up = normalize(info.up) ?? [0, -1, 0];
  const rotation = rotationBetween(up, [0, 1, 0]);
  const radius = Number.isFinite(info.radius) && info.radius > 0 ? info.radius : 1;

  // モデル上の点 p は、ビューアでは rotate(p) + position に来る。
  // center が原点に来てほしいので position = -rotate(center)。
  const rc = rotate(rotation, info.center);
  const position: Vec3 = [-rc[0], -rc[1], -rc[2]];

  return {
    rotation,
    position,
    cameraPosition: initialCamera(info, rotation, position, radius),
    radius,
    approximate: false,
  };
}

/**
 * カメラの初期位置を決める。
 *
 * 実際に写真を撮った位置の1つ（1枚目）を使う。そこからは必ず被写体が写っているので、
 * 「開いたら何も見えない」「裏側から見ている」といった事故が起きない。
 * 3DGS は撮った位置から見たときに一番正しく見えるので、できるだけその位置のまま使う。
 * 極端に近い・遠い場合だけ、中心からの距離を見やすい範囲に収める。
 * （radius は点の広がりで、床や背景の点が多いと被写体より大きくなる。1周の動画では、
 *   撮影位置までの距離が radius の 0.7 倍ほどのことがあった。下限は radius の半分にしてある。）
 */
function initialCamera(info: SceneInfo, rotation: Quat, position: Vec3, radius: number): Vec3 {
  const first = info.cameraPositions[0];
  if (!first) return [0, 0, radius * 3];
  const r = rotate(rotation, first);
  const p: Vec3 = [r[0] + position[0], r[1] + position[1], r[2] + position[2]];
  const dir = normalize(p);
  if (!dir) return [0, 0, radius * 3];
  const distance = Math.min(Math.max(length(p), radius * 0.5), radius * 8);
  return [dir[0] * distance, dir[1] * distance, dir[2] * distance];
}

/** モデル上の点（COLMAP の座標）を、ビューアの座標に直す。 */
export function toView(frame: ViewFrame, p: Vec3): Vec3 {
  const r = rotate(frame.rotation, p);
  return [r[0] + frame.position[0], r[1] + frame.position[1], r[2] + frame.position[2]];
}
