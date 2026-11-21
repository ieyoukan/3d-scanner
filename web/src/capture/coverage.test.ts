import { describe, expect, it } from 'vitest';
import { currentTile, directionOf, isFacing, nextTarget, progressOf, tileAt, tiles } from './coverage';

const O = [0, 0, 0] as const;

describe('tiles', () => {
  it('横 12・上 6・真上 1', () => {
    expect(tiles.length).toBe(19);
    expect(tiles.filter((t) => t.band === 'side').length).toBe(12);
  });
});

describe('directionOf / tileAt', () => {
  it('+z が方位 0°、+x が 90°、真上は仰角 90°', () => {
    expect(directionOf(O, [0, 0, 2])).toEqual({ az: 0, el: 0 });
    expect(directionOf(O, [2, 0, 0]).az).toBeCloseTo(90);
    expect(directionOf(O, [0, 1, 0]).el).toBeCloseTo(90);
  });

  it('方向からタイルを決める。真下の方向はドームの外', () => {
    expect(tileAt(0, 0)).toBe(0);
    expect(tileAt(95, 10)).toBe(3);
    expect(tileAt(359.9, 29.9)).toBe(11);
    expect(tileAt(100, 40)).toBe(13); // 上の2枚目（60〜120°）
    expect(tileAt(200, 80)).toBe(18); // 真上
    expect(tileAt(0, 90)).toBe(18);
    expect(tileAt(0, -30)).toBeNull();
  });
});

describe('isFacing / currentTile', () => {
  it('対象を向いているときだけ、いまのタイルを返す', () => {
    expect(isFacing([0, 0, 2], [0, 0, -1], O)).toBe(true);
    expect(isFacing([0, 0, 2], [1, 0, 0], O)).toBe(false);
    expect(currentTile([0, 0, 2], [0, 0, -1], O)).toBe(0);
    expect(currentTile([0, 0, 2], [0, 0, 1], O)).toBeNull();
  });
});

describe('nextTarget', () => {
  const none = () => tiles.map(() => false);

  it('いまの方向がまだなら、そのまま撮る', () => {
    expect(nextTarget(none(), [0, 0, 2], O)).toEqual({ tile: 0, turn: 'here' });
  });

  it('いまの方向が済んでいたら、一番近いまだの方向へ（+z に立って中心を見ると、右が +x）', () => {
    const c = none();
    c[0] = true;
    // 方位 0° に立っている。左隣（330〜360°、-x 側）が一番近い。
    expect(nextTarget(c, [0, 0, 2], O)).toEqual({ tile: 11, turn: 'left' });
    c[11] = true;
    c[10] = true;
    // 次は右隣（30〜60°、+x 側）。
    expect(nextTarget(c, [0, 0, 2], O)).toEqual({ tile: 1, turn: 'right' });
  });

  it('横の1周が済んだら上、最後に真上。全部済んだら null', () => {
    const c = tiles.map((t) => t.band === 'side');
    expect(nextTarget(c, [0, 0, 2], O)).toEqual({ tile: 12, turn: 'up' });
    const all = tiles.map((t) => t.band !== 'top');
    expect(nextTarget(all, [0, 0, 2], O)).toEqual({ tile: 18, turn: 'up' });
    expect(nextTarget(tiles.map(() => true), [0, 0, 2], O)).toBeNull();
  });
});

describe('progressOf', () => {
  it('帯ごとに数える', () => {
    const c = tiles.map((_, i) => i < 5 || i === 12 || i === 18);
    expect(progressOf(c)).toEqual({ side: 5, sideTotal: 12, upper: 1, upperTotal: 6, top: true });
  });
});
