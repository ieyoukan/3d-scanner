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
