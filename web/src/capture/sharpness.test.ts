import { describe, expect, it } from 'vitest';
import { findBlurry, laplacianVariance, toGray } from './sharpness';

/** 市松模様（くっきりした画像の代表）を作る。 */
function checkerboard(size: number, cell: number): Float32Array {
  const gray = new Float32Array(size * size);
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      gray[y * size + x] = (Math.floor(x / cell) + Math.floor(y / cell)) % 2 === 0 ? 255 : 0;
    }
  }
  return gray;
}

/** 3×3 の平均でぼかす（ピンぼけの代わり）。 */
function blur(gray: Float32Array, size: number): Float32Array {
  const out = new Float32Array(gray.length);
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let sum = 0;
      let n = 0;
      for (let dy = -1; dy <= 1; dy++) {
        for (let dx = -1; dx <= 1; dx++) {
          const xx = x + dx;
          const yy = y + dy;
          if (xx >= 0 && yy >= 0 && xx < size && yy < size) {
            sum += gray[yy * size + xx] ?? 0;
            n++;
          }
        }
      }
      out[y * size + x] = sum / n;
    }
  }
  return out;
}

describe('laplacianVariance', () => {
  it('一様な画像は 0（輪郭が無い）', () => {
    expect(laplacianVariance(new Float32Array(64 * 64).fill(128), 64, 64)).toBe(0);
  });

  it('くっきりした画像は、同じ画像をぼかしたものより大きい', () => {
    const sharp = checkerboard(64, 4);
    const once = blur(sharp, 64);
    const twice = blur(once, 64);
    const a = laplacianVariance(sharp, 64, 64);
    const b = laplacianVariance(once, 64, 64);
    const c = laplacianVariance(twice, 64, 64);
    expect(a).toBeGreaterThan(b);
    expect(b).toBeGreaterThan(c);
    expect(c).toBeGreaterThan(0);
  });

  it('小さすぎる画像は 0 を返す（上下左右の画素が揃わない）', () => {
    expect(laplacianVariance(new Float32Array(4), 2, 2)).toBe(0);
  });
});

describe('toGray', () => {
  it('RGBA を明るさに変換する（緑が最も明るく、青が最も暗い）', () => {
    const gray = toGray([255, 0, 0, 255, 0, 255, 0, 255, 0, 0, 255, 255, 255, 255, 255, 255]);
    expect(gray).toHaveLength(4);
    const [r, g, b, w] = gray;
    expect(g).toBeGreaterThan(r as number);
    expect(r).toBeGreaterThan(b as number);
    expect(w).toBeCloseTo(255, 3);
  });
});

describe('findBlurry', () => {
  const frames = (values: number[]) => values.map((sharpness, i) => ({ id: `f${i}`, sharpness }));

  it('動画から選んだコマは比べない（模様の少ないコマを誤って「ぼけ」にしない）', () => {
    const video = [100, 100, 100, 100, 100, 10].map((sharpness, i) => ({ id: `v${i}`, sharpness, source: 'video' }));
    expect(findBlurry(video).size).toBe(0);
    // 写真と混ざっていても、写真どうしだけで比べる。
    const mixed = [...frames([100, 100, 100, 100, 10]), ...video];
    expect([...findBlurry(mixed)]).toEqual(['f4']);
  });

  it('他と比べて極端に低い写真だけを選ぶ', () => {
    const blurry = findBlurry(frames([100, 110, 95, 105, 20, 98]));
    expect([...blurry]).toEqual(['f4']);
  });

  it('全体が同じ程度なら、値が低くても選ばない（無地の被写体を誤判定しない）', () => {
    expect(findBlurry(frames([8, 9, 7, 8, 9, 8])).size).toBe(0);
  });

  it('枚数が少ないと比較できないので選ばない', () => {
    expect(findBlurry(frames([100, 100, 100, 1])).size).toBe(0);
  });

  it('極端に高い写真が混ざっても、基準（中央値）は引っ張られない', () => {
    const blurry = findBlurry(frames([100, 100, 100, 100, 100000, 90]));
    expect(blurry.size).toBe(0);
  });
});
