import { describe, expect, it } from 'vitest';
import { BestPerWindow, planCount, windowOf } from './video';

describe('planCount', () => {
  it('1秒に3枚。残りの枚数を超えない', () => {
    expect(planCount(12.2, 80)).toBe(37);
    expect(planCount(60, 80)).toBe(80);
    expect(planCount(12.2, 10)).toBe(10);
  });

  it('長さが分からない・追加できないなら 0', () => {
    expect(planCount(Number.NaN, 80)).toBe(0);
    expect(planCount(Infinity, 80)).toBe(0);
    expect(planCount(10, 0)).toBe(0);
  });
});

describe('windowOf', () => {
  it('同じ長さの区間に分け、最後の瞬間は最後の区間に入れる', () => {
    expect(windowOf(0, 10, 5)).toBe(0);
    expect(windowOf(1.99, 10, 5)).toBe(0);
    expect(windowOf(2, 10, 5)).toBe(1);
    expect(windowOf(10, 10, 5)).toBe(4);
    expect(windowOf(-1, 10, 5)).toBe(0);
  });
});

describe('BestPerWindow', () => {
  it('区間で一番くっきりしたコマだけを控え、次の区間に進んだら確定する', () => {
    const p = new BestPerWindow(3, 3); // 1秒ずつ3区間
    expect(p.offer(0.0, 10)).toEqual({ window: 0, keep: true, closed: [] });
    expect(p.offer(0.3, 5)).toEqual({ window: 0, keep: false, closed: [] });
    expect(p.offer(0.6, 20)).toEqual({ window: 0, keep: true, closed: [] });
    expect(p.offer(1.1, 1)).toEqual({ window: 1, keep: true, closed: [0] });
    expect(p.offer(1.5, 3)).toEqual({ window: 1, keep: true, closed: [] });
    // 区間を飛ばしても（コマが落ちても）、それまでの区間はまとめて確定する。
    expect(p.offer(2.5, 1)).toEqual({ window: 2, keep: true, closed: [1] });
    expect(p.finish()).toEqual([2]);
    expect(p.finish()).toEqual([]);
  });

  it('確定した区間に戻ってきたコマは使わない', () => {
    const p = new BestPerWindow(2, 2);
    p.offer(0.1, 1);
    p.offer(1.2, 1);
    expect(p.offer(0.2, 100)).toEqual({ window: 0, keep: false, closed: [] });
  });

  it('コマが1枚も来なかった区間は、後から埋められる', () => {
    const p = new BestPerWindow(4, 4);
    p.offer(0.5, 1);
    expect(p.offer(3.5, 1).closed).toEqual([0]);
    expect(p.finish()).toEqual([3]);
    expect(p.missing()).toEqual([1, 2]);
    // 再生の後で区間の中央へ移動して見せたコマは、確定済みの区間より前でも受け取る。
    expect(p.offer(1.5, 1).keep).toBe(true);
    expect(p.finish()).toEqual([1]);
    expect(p.missing()).toEqual([2]);
  });
});
