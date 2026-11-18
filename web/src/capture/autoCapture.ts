// 自動撮影（連写）の判断。画面にも React にも依存しない純粋な関数だけを置く（autoCapture.test.ts）。
//
// 狙い: 「一部しか復元できない」の多くは、隣り合う写真の重なりが足りないか、ぶれていることが原因。
// 人がシャッターを押す間隔はばらつくので、アプリが映像を見て次の条件がそろった瞬間に撮る:
//
//   1. 止まっている     … 直前のコマとの違いが小さい状態が少し続いている（ぶれにくい）
//   2. 景色が変わった   … 前に撮った写真との違いが十分ある（同じ写真を何枚も撮らない）
//   3. 間隔が空いている … 前に撮ってから一定時間が経っている
//
// 「違い」の測り方: 映像を 32×24 の白黒に縮め、明るさの平均 0・ばらつき 1 にそろえてから、
// 画素ごとの差の絶対値を平均する。照明の明るさが少し変わっても反応しにくい。
// 無関係な2枚では約 1.13（標準正規分布2つの差の絶対値の平均 2/√π）になるので、それで割って
// 「0 = 同じ、1 = まったく別の景色」の目安にそろえる。
//
// 手持ちでは完全には止まれないので、「止まっている」の基準は2段にする:
//   ふつうは steady（200ms で画面幅の約1.2%以下の動き）が steadyMs 続いたら撮る。
//   景色が十分変わってから looseAfterMs 経っても撮れていなければ、looseSteady（約2%）まで許して撮る。
//   これで「手が震えていつまでも撮れない」を防ぐ。2% / 200ms は露光 1/60 秒で幅の約0.17%のぶれで、位置合わせには支障がない。
//
// 注意: しきい値は実写真をずらして測った値から決めた（実機の手ぶれ・自動露出での調整はまだ）。

export const signatureWidth = 32;
export const signatureHeight = 24;

/** 映像1コマの要約（32×24、明るさをそろえた白黒）。flat は模様がほとんど無いこと。 */
export interface Signature {
  data: Float32Array;
  flat: boolean;
}

/** 白黒画像（width×height）を、面積平均で 32×24 に縮めて要約にする。 */
export function signatureOf(gray: ArrayLike<number>, width: number, height: number): Signature {
  const w = signatureWidth;
  const h = signatureHeight;
  const data = new Float32Array(w * h);
  for (let y = 0; y < h; y++) {
    const y0 = Math.floor((y * height) / h);
    const y1 = Math.max(y0 + 1, Math.floor(((y + 1) * height) / h));
    for (let x = 0; x < w; x++) {
      const x0 = Math.floor((x * width) / w);
      const x1 = Math.max(x0 + 1, Math.floor(((x + 1) * width) / w));
      let sum = 0;
      for (let yy = y0; yy < y1; yy++) {
        for (let xx = x0; xx < x1; xx++) sum += gray[yy * width + xx] ?? 0;
      }
      data[y * w + x] = sum / ((y1 - y0) * (x1 - x0));
    }
  }
  let mean = 0;
  for (const v of data) mean += v;
  mean /= data.length;
  let variance = 0;
  for (const v of data) variance += (v - mean) ** 2;
  const std = Math.sqrt(variance / data.length);
  // 0〜255 の明るさで標準偏差 4 未満は、ほぼ無地（白い壁、机だけ、レンズを覆っている）。
  const flat = std < 4;
  for (let i = 0; i < data.length; i++) data[i] = flat ? 0 : ((data[i] ?? 0) - mean) / std;
  return { data, flat };
}

/** 独立な2枚の差の絶対値の平均（2/√π）。これで割って 0〜1 程度の目安にする。 */
const unrelatedMAD = 2 / Math.sqrt(Math.PI);

/** 2つの要約の違い。0 = 同じ、1 前後 = 無関係な景色。 */
export function difference(a: Signature, b: Signature): number {
  let sum = 0;
  for (let i = 0; i < a.data.length; i++) sum += Math.abs((a.data[i] ?? 0) - (b.data[i] ?? 0));
  return sum / a.data.length / unrelatedMAD;
}

export interface AutoConfig {
  /** 直前のコマとの違いがこれ未満なら「止まっている」。 */
  steady: number;
  /** 止まっている状態がこれだけ続いたら撮ってよい（ミリ秒）。 */
  steadyMs: number;
  /** 前に撮った写真との違いがこれ以上なら「景色が変わった」。 */
  minChange: number;
  /** 前に撮った写真との違いがこれを超えたら「離れすぎ」（重なりが足りないおそれ）。 */
  tooFar: number;
  /** 撮影の最小間隔（ミリ秒）。 */
  minIntervalMs: number;
  /** 景色が十分変わってから、これだけ待っても撮れなければ、基準を looseSteady まで緩める（ミリ秒）。 */
  looseAfterMs: number;
  /** 緩めたときの「止まっている」の基準。 */
  looseSteady: number;
}

// 目安（サンプル写真11枚で測った値）:
//   写真を画面幅の 0.5% / 1% / 2% / 4% ずらしたときの違いは、およそ 0.06 / 0.11 / 0.19 / 0.30。
//   1周しながら撮った隣り合う写真どうしの違いは 0.16〜0.85（COLMAP が全部の位置を推定できた）。
// minChange はその最小付近（幅の約2%の移動）にし、tooFar は「ほぼ別の景色」のときだけ知らせる値にした
// （この測り方は景色の全体の見た目を比べるだけなので、違いが大きくても特徴点は対応づくことが多い）。
export const defaultAutoConfig: AutoConfig = {
  steady: 0.13,
  steadyMs: 150,
  minChange: 0.17,
  tooFar: 0.9,
  minIntervalMs: 600,
  looseAfterMs: 1000,
  looseSteady: 0.22,
};

export interface AutoState {
  /** 直前に見たコマ。 */
  prev: Signature | null;
  /** 止まり始めた時刻（止まっていなければ null）。 */
  steadySince: number | null;
  /** 前に撮った写真。 */
  last: Signature | null;
  lastAt: number;
  /** 景色が十分変わって「止まれば撮る」状態になった時刻（なっていなければ null）。 */
  readySince: number | null;
}

export const initialAutoState: AutoState = {
  prev: null,
  steadySince: null,
  last: null,
  lastAt: -Infinity,
  readySince: null,
};

/**
 * 次にどうするか。
 *   capture  いま撮る
 *   move     前の写真からまだ景色が変わっていない（横に動いてほしい）
 *   stop     景色は十分変わった（止まれば撮る）
 *   flat     模様が無い（位置合わせの手掛かりが無い）
 */
export type AutoAdvice = 'capture' | 'move' | 'stop' | 'flat';

export interface AutoDecision {
  state: AutoState;
  advice: AutoAdvice;
  /** capture のとき、前の写真から離れすぎていたか（間を埋める写真を撮るよう促す）。 */
  tooFar: boolean;
  /** 次の1枚までの進み具合（0〜1）。前の写真からの景色の変わり具合を minChange で割った値。 */
  progress: number;
}

/** 映像の1コマを見て、次にどうするかを決める。 */
export function decide(state: AutoState, sig: Signature, now: number, cfg: AutoConfig = defaultAutoConfig): AutoDecision {
  const motion = state.prev ? difference(state.prev, sig) : Infinity;
  const steadySince = motion < cfg.steady ? (state.steadySince ?? now) : null;
  const change = state.last ? difference(state.last, sig) : Infinity;
  const progress = Math.min(1, change / cfg.minChange);
  const enough = change >= cfg.minChange && now - state.lastAt >= cfg.minIntervalMs;
  const readySince = enough && !sig.flat ? (state.readySince ?? now) : null;
  const next: AutoState = { ...state, prev: sig, steadySince, readySince };
  const result = (advice: AutoAdvice, tooFar = false): AutoDecision => ({ state: next, advice, tooFar, progress });

  if (sig.flat) return result('flat');
  if (readySince === null) return result('move');
  const steady = steadySince !== null && now - steadySince >= cfg.steadyMs;
  // 止まりきれない手でも撮れるよう、待ちが長引いたら基準を緩める。
  const steadyEnough = steady || (now - readySince >= cfg.looseAfterMs && motion < cfg.looseSteady);
  if (!steadyEnough) return result('stop');
  return result('capture', state.last !== null && change > cfg.tooFar);
}

/** 撮ったことを記録する（次からは、この写真との違いで「景色が変わったか」を見る）。 */
export function markCaptured(state: AutoState, sig: Signature, now: number): AutoState {
  return { ...state, last: sig, lastAt: now, readySince: null };
}
