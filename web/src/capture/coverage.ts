// AR で撮るときの「どの方向から撮れたか」の判定と、次に撮る方向の案内（画面に依存しない計算）。
//
// 対象のまわりに半球（ドーム）を考え、方向ごとのタイルに分ける:
//   横    仰角 -15°〜30°、30°ごとに 12 枚（1周。まずここを全部埋める）
//   上    仰角 30°〜65°、60°ごとに 6 枚（上から見た面のため）
//   真上  仰角 65°〜90°、1 枚
// カメラが対象のほうを向いて（視線と対象の方向のずれが maxFacingDeg 以内）撮ったとき、
// カメラのいる方向のタイルを「撮れた」にする。
//
// 座標は WebXR と同じ: y が上、長さはメートル。方位角は真上から見て +z 方向を 0° とし、atan2(x, z)。

export type Vec3 = readonly [number, number, number];

export type Band = 'side' | 'upper' | 'top';

export interface Tile {
  band: Band;
  /** 方位角の範囲（度。azTo > azFrom、0〜360）。 */
  azFrom: number;
  azTo: number;
  /** 仰角の範囲（度）。 */
  elFrom: number;
  elTo: number;
}

const SIDE_COUNT = 12;
const UPPER_COUNT = 6;
const SIDE_EL: [number, number] = [-15, 30];
const UPPER_EL: [number, number] = [30, 65];
const TOP_EL: [number, number] = [65, 90];

function ring(band: Band, count: number, el: [number, number]): Tile[] {
  const step = 360 / count;
  return Array.from({ length: count }, (_, i) => ({
    band,
    azFrom: i * step,
    azTo: (i + 1) * step,
    elFrom: el[0],
    elTo: el[1],
  }));
}

/** すべてのタイル（横 12 → 上 6 → 真上 1 の順）。 */
export const tiles: readonly Tile[] = [
  ...ring('side', SIDE_COUNT, SIDE_EL),
  ...ring('upper', UPPER_COUNT, UPPER_EL),
  { band: 'top', azFrom: 0, azTo: 360, elFrom: TOP_EL[0], elTo: TOP_EL[1] },
];

/** 対象を向いているとみなす、視線と「対象への方向」のずれの上限（度）。 */
export const maxFacingDeg = 40;

const deg = (rad: number) => (rad * 180) / Math.PI;

/** 角度の差を -180〜180 に収める。 */
export function wrapDeg(a: number): number {
  return ((((a + 180) % 360) + 360) % 360) - 180;
}

/** 中心から見たカメラの方向（方位角 0〜360、仰角 -90〜90。度）。 */
export function directionOf(center: Vec3, pos: Vec3): { az: number; el: number } {
  const dx = pos[0] - center[0];
  const dy = pos[1] - center[1];
  const dz = pos[2] - center[2];
  const horiz = Math.hypot(dx, dz);
  const az = (deg(Math.atan2(dx, dz)) + 360) % 360;
  const el = deg(Math.atan2(dy, horiz));
  return { az, el };
}

/** 方向が入るタイルの番号。ドームの外（真下の方向など）なら null。 */
export function tileAt(az: number, el: number): number | null {
  const a = ((az % 360) + 360) % 360;
  for (let i = 0; i < tiles.length; i++) {
    const t = tiles[i]!;
    if (el >= t.elFrom && (el < t.elTo || (t.band === 'top' && el <= 90)) && a >= t.azFrom && a < t.azTo) return i;
  }
  return null;
}

/** カメラ（位置 pos、視線 forward）が中心を向いているか。 */
export function isFacing(pos: Vec3, forward: Vec3, center: Vec3, maxDeg = maxFacingDeg): boolean {
  const to: Vec3 = [center[0] - pos[0], center[1] - pos[1], center[2] - pos[2]];
  const lt = Math.hypot(...to);
  const lf = Math.hypot(...forward);
  if (lt < 1e-6 || lf < 1e-6) return false;
  const cos = (to[0] * forward[0] + to[1] * forward[1] + to[2] * forward[2]) / (lt * lf);
  return deg(Math.acos(Math.min(1, Math.max(-1, cos)))) <= maxDeg;
}

/** いまカメラが撮っているタイル（中心を向いていなければ null）。 */
export function currentTile(pos: Vec3, forward: Vec3, center: Vec3): number | null {
  if (!isFacing(pos, forward, center)) return null;
  const { az, el } = directionOf(center, pos);
  return tileAt(az, el);
}

export interface Progress {
  side: number;
  sideTotal: number;
  upper: number;
  upperTotal: number;
  top: boolean;
}

export function progressOf(covered: readonly boolean[]): Progress {
  let side = 0;
  let upper = 0;
  tiles.forEach((t, i) => {
    if (!covered[i]) return;
    if (t.band === 'side') side++;
    if (t.band === 'upper') upper++;
  });
  return { side, sideTotal: SIDE_COUNT, upper, upperTotal: UPPER_COUNT, top: !!covered[tiles.length - 1] };
}

/**
 * 次に撮るところと、そこへの行き方。
 *   here   いまの方向がまだ撮れていない（そのまま撮る）
 *   left / right  対象を向いたまま、左 / 右へ回る
 *   up     いまの位置で、もっと上から撮る
 * 全部撮れていれば null。横の1周を先に埋め、終わったら上、最後に真上を案内する。
 */
export function nextTarget(
  covered: readonly boolean[],
  pos: Vec3,
  center: Vec3,
): { tile: number; turn: 'here' | 'left' | 'right' | 'up' } | null {
  const { az, el } = directionOf(center, pos);
  const here = tileAt(az, el);
  for (const band of ['side', 'upper', 'top'] as const) {
    const open = tiles.map((t, i) => ({ t, i })).filter(({ t, i }) => t.band === band && !covered[i]);
    if (open.length === 0) continue;
    if (here !== null && open.some(({ i }) => i === here)) return { tile: here, turn: 'here' };
    // 方位角の差が一番小さいタイル。
    let best = open[0]!;
    let bestDelta = Infinity;
    for (const o of open) {
      const delta = band === 'top' ? 0 : wrapDeg((o.t.azFrom + o.t.azTo) / 2 - az);
      if (Math.abs(delta) < Math.abs(bestDelta)) {
        best = o;
        bestDelta = delta;
      }
    }
    // 同じ方位で高さだけが違う（横は済んで、上が残っている）なら、上へ。
    const halfWidth = (best.t.azTo - best.t.azFrom) / 2;
    if (band !== 'side' && (band === 'top' || Math.abs(bestDelta) <= halfWidth)) {
      return { tile: best.i, turn: 'up' };
    }
    return { tile: best.i, turn: turnToward(pos, center, bestDelta) };
  }
  return null;
}

/** 撮影中のカメラの位置と視線（1コマ分）。 */
export interface PoseSample {
  pos: Vec3;
  fwd: Vec3;
}

/** 撮ってきたカメラの位置と視線から、タイルごとの「撮れた」を数え直す（中心を動かしたとき用）。 */
export function coverageOf(samples: readonly PoseSample[], center: Vec3): boolean[] {
  const covered = tiles.map(() => false);
  for (const s of samples) {
    const i = currentTile(s.pos, s.fwd, center);
    if (i !== null) covered[i] = true;
  }
  return covered;
}

/** 視線の集まる点を信じてよい、視線の広がりの下限（det(A/n)。2本の視線なら約 27° 開いたとき）。 */
export const minSpread = 0.05;

/**
 * カメラの視線がいちばん集まる点（各視線の直線までの距離の二乗の和が最小の点）。
 * 対象を画面の真ん中に入れて回ると、対象の中心になる（サーバーの focusPoint と同じ考え方）。
 *
 * 次のときは null（まだ決められない）:
 *   - 視線の向きが揃いすぎている（回った角度が小さい。広がりが minSpread 未満）
 *   - 求めた点が、8 割以上のカメラの前に無い（視線がほぼ平行で、後ろに解が出た等）
 * よそ見した視線に引っ張られないよう、一度解いてから、解から遠く外れた視線を除いて解き直す。
 */
export function convergePoint(samples: readonly PoseSample[]): Vec3 | null {
  const rays = samples.flatMap((s) => {
    const l = Math.hypot(...s.fwd);
    return l > 1e-6 ? [{ c: s.pos, d: [s.fwd[0] / l, s.fwd[1] / l, s.fwd[2] / l] as Vec3 }] : [];
  });
  let x = solveRays(rays);
  if (!x) return null;
  const dist = rays.map((r) => rayDistance(r, x!));
  const median = [...dist].sort((a, b) => a - b)[Math.floor(dist.length / 2)]!;
  const limit = Math.max(0.05, 3 * median);
  const inliers = rays.filter((_, i) => dist[i]! <= limit);
  if (inliers.length < rays.length) x = solveRays(inliers);
  if (!x) return null;
  const p = x;
  const inFront = inliers.filter((r) => dot(r.d, [p[0] - r.c[0], p[1] - r.c[1], p[2] - r.c[2]]) > 0).length;
  return inFront >= 0.8 * inliers.length ? p : null;
}

interface Ray {
  c: Vec3;
  d: Vec3;
}

const dot = (a: Vec3, b: Vec3) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];

/** 点 x から直線（c を通り向き d、|d|=1）までの距離。 */
function rayDistance(r: Ray, x: Vec3): number {
  const v: Vec3 = [x[0] - r.c[0], x[1] - r.c[1], x[2] - r.c[2]];
  const t = dot(v, r.d);
  return Math.hypot(v[0] - t * r.d[0], v[1] - t * r.d[1], v[2] - t * r.d[2]);
}

/** Σ(I − ddᵀ) x = Σ(I − ddᵀ) c を解く。視線の広がりが足りなければ null。 */
function solveRays(rays: readonly Ray[]): Vec3 | null {
  const n = rays.length;
  if (n < 2) return null;
  const A = [0, 0, 0, 0, 0, 0, 0, 0, 0];
  const b = [0, 0, 0];
  for (const { c, d } of rays) {
    for (let i = 0; i < 3; i++) {
      for (let j = 0; j < 3; j++) {
        const m = (i === j ? 1 : 0) - d[i]! * d[j]!;
        A[i * 3 + j]! += m;
        b[i]! += m * c[j]!;
      }
    }
  }
  const det = det3(A);
  if (det / n ** 3 < minSpread) return null;
  // クラメルの公式。
  const col = (k: number) => A.map((v, idx) => (idx % 3 === k ? b[Math.floor(idx / 3)]! : v));
  return [det3(col(0)) / det, det3(col(1)) / det, det3(col(2)) / det];
}

function det3(m: readonly number[]): number {
  const [a, b, c, d, e, f, g, h, i] = m as [number, number, number, number, number, number, number, number, number];
  return a * (e * i - f * h) - b * (d * i - f * g) + c * (d * h - e * g);
}

/**
 * 方位角を delta 度変えるには、対象を向いたまま左右どちらへ回ればよいか。
 * 右へ少し動いたときに方位角が増えるか減るかを実際に計算して決める（座標系の向きの思い込みを避ける）。
 */
function turnToward(pos: Vec3, center: Vec3, delta: number): 'left' | 'right' {
  const fx = center[0] - pos[0];
  const fz = center[2] - pos[2];
  const len = Math.hypot(fx, fz) || 1;
  // 視線 f と上 (0,1,0) の外積 f × up = (-fz, 0, fx) が右方向。
  const right: Vec3 = [-fz / len, 0, fx / len];
  const step = 0.01;
  const moved: Vec3 = [pos[0] + right[0] * step, pos[1], pos[2] + right[2] * step];
  const dAz = wrapDeg(directionOf(center, moved).az - directionOf(center, pos).az);
  return Math.sign(dAz) === Math.sign(delta) ? 'right' : 'left';
}
