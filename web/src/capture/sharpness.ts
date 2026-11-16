// 写真の「くっきり度」を数値にする（ぼけ・ぶれの検出）。
//
// 方法: ラプラシアンの分散。
//   ラプラシアンは「ある画素と、上下左右の画素との差」を取る演算（2階微分の近似）。
//     L(x, y) = 上 + 下 + 左 + 右 − 4 × 中央
//   輪郭や模様がくっきり写っていると、この値は場所によって大きく振れる（分散が大きい）。
//   ぼけた写真は隣り合う画素の差が小さいので、どこでも 0 に近くなる（分散が小さい）。
//
// SfM は写真の中の「角や模様」を手掛かりにするので、ぼけた写真はほとんど役に立たない。
// 送る前に見つけて外せれば、処理時間の無駄と失敗を減らせる。
//
// 注意: この値は被写体の模様の細かさにも左右される（無地の壁はくっきり撮っても低い）。
// だから絶対値では判定せず、「同じ撮影の他の写真と比べて極端に低いか」を見る（findBlurry）。

/**
 * グレースケール画像のラプラシアンの分散を返す。
 * @param gray 各画素の明るさ（0〜255）を左上から右へ、上から下へ並べた配列
 */
export function laplacianVariance(gray: ArrayLike<number>, width: number, height: number): number {
  if (width < 3 || height < 3) return 0;
  let sum = 0;
  let sumSq = 0;
  let count = 0;
  // 端の画素は上下左右が揃わないので除く。
  for (let y = 1; y < height - 1; y++) {
    for (let x = 1; x < width - 1; x++) {
      const i = y * width + x;
      const lap =
        (gray[i - width] ?? 0) + (gray[i + width] ?? 0) + (gray[i - 1] ?? 0) + (gray[i + 1] ?? 0) - 4 * (gray[i] ?? 0);
      sum += lap;
      sumSq += lap * lap;
      count++;
    }
  }
  const mean = sum / count;
  // 分散 = 二乗の平均 − 平均の二乗
  return sumSq / count - mean * mean;
}

/** RGBA の画素列（canvas の getImageData）をグレースケールに変換する。 */
export function toGray(rgba: ArrayLike<number>): Float32Array {
  const gray = new Float32Array(rgba.length / 4);
  for (let i = 0; i < gray.length; i++) {
    // 人の目は緑に最も敏感で青に鈍い。その感度に合わせた重み（ITU-R BT.601）。
    gray[i] = 0.299 * (rgba[i * 4] ?? 0) + 0.587 * (rgba[i * 4 + 1] ?? 0) + 0.114 * (rgba[i * 4 + 2] ?? 0);
  }
  return gray;
}

/** 比較に必要な最小枚数。少なすぎると「他と比べて」が成り立たない。 */
const minFramesToCompare = 5;
/** 中央値のこの割合を下回ったら「ぼけているかもしれない」とみなす。 */
const blurryRatio = 0.35;

/**
 * 他の写真と比べて極端にくっきり度が低い写真の id を返す。
 * 基準に平均ではなく中央値を使うのは、極端に高い・低い写真に基準が引っ張られないようにするため。
 */
export function findBlurry(frames: readonly { id: string; sharpness: number }[]): Set<string> {
  const blurry = new Set<string>();
  if (frames.length < minFramesToCompare) return blurry;
  const sorted = frames.map((f) => f.sharpness).sort((a, b) => a - b);
  const median = sorted[Math.floor(sorted.length / 2)] ?? 0;
  if (median <= 0) return blurry;
  for (const f of frames) {
    if (f.sharpness < median * blurryRatio) blurry.add(f.id);
  }
  return blurry;
}
