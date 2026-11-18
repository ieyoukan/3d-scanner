import { describe, expect, it } from 'vitest';
import { decide, difference, initialAutoState, markCaptured, signatureOf, type AutoState } from './autoCapture';

// 合成の景色: なめらかな模様（実際の景色のように、近い所は似ている）を横に長く作り、窓として切り出す。
// 窓をずらすと「横に移動した」ことになる。
const W = 64;
const H = 48;
function scene(seed: number) {
  const value = (x: number, y: number) =>
    128 +
    40 * Math.sin(x / 9 + seed) +
    30 * Math.sin(y / 7 + x / 23 + seed * 2) +
    25 * Math.sin(x / 31 - y / 11 + seed * 3) +
    15 * Math.sin(x / 5.3 + y / 13);
  return (offset: number) => {
    const out = new Float32Array(W * H);
    for (let y = 0; y < H; y++) for (let x = 0; x < W; x++) out[y * W + x] = value(x + offset, y);
    return signatureOf(out, W, H);
  };
}

describe('difference', () => {
  const view = scene(1);
  it('同じなら 0、少しずらすと小さく、大きくずらすと 1 に近い', () => {
    const a = view(0);
    expect(difference(a, view(0))).toBe(0);
    const small = difference(a, view(1));
    const large = difference(a, view(200));
    expect(small).toBeGreaterThan(0);
    expect(small).toBeLessThan(difference(a, view(4)));
    expect(difference(a, view(4))).toBeLessThan(large);
    expect(large).toBeGreaterThan(0.9);
  });

  it('明るさが全体に変わっても、ほとんど反応しない', () => {
    const base = new Float32Array(W * H).map((_, i) => (i * 37) % 200);
    const brighter = base.map((v) => v * 1.2 + 10);
    expect(difference(signatureOf(base, W, H), signatureOf(brighter, W, H))).toBeLessThan(0.01);
  });

  it('無地は flat', () => {
    expect(signatureOf(new Float32Array(W * H).fill(128), W, H).flat).toBe(true);
    expect(scene(2)(0).flat).toBe(false);
  });
});

describe('decide', () => {
  const view = scene(3);
  // 一定間隔でコマを見せたときの判断を順に返す。
  function run(offsets: number[], startState: AutoState = initialAutoState) {
    let state = startState;
    const advices: string[] = [];
    offsets.forEach((offset, i) => {
      const now = i * 200;
      const sig = view(offset);
      const d = decide(state, sig, now);
      state = d.state;
      advices.push(d.advice + (d.tooFar ? '!' : ''));
      if (d.advice === 'capture') state = markCaptured(state, sig, now);
    });
    return advices;
  }

  it('止まったら1枚目を撮る。同じ景色のままなら撮らず、移動を促す', () => {
    // 1コマ目は比べる相手がいないので待つ。2コマ目から止まり始め、0.25 秒続いた所で撮る。
    expect(run([0, 0, 0, 0, 0, 0, 0, 0])).toEqual(['hold', 'hold', 'hold', 'capture', 'hold', 'hold', 'hold', 'move']);
  });

  it('動いている間は撮らず、止まって景色が変わっていたら撮る', () => {
    const advices = run([0, 0, 0, 0, 2, 4, 6, 6, 6, 6]);
    expect(advices.slice(0, 4)).toEqual(['hold', 'hold', 'hold', 'capture']);
    expect(advices.slice(4, 7)).toEqual(['hold', 'hold', 'hold']); // 移動中
    expect(advices.at(-1)).toBe('capture');
  });

  it('前の写真から離れすぎてから撮ると、そのことを知らせる', () => {
    const advices = run([0, 0, 0, 0, 8, 16, 24, 24, 24, 24]);
    expect(advices.at(-1)).toBe('capture!');
  });

  it('無地なら撮らない', () => {
    const flat = signatureOf(new Float32Array(W * H).fill(90), W, H);
    let state = initialAutoState;
    for (let i = 0; i < 6; i++) {
      const d = decide(state, flat, i * 200);
      expect(d.advice).toBe('flat');
      state = d.state;
    }
  });
});
