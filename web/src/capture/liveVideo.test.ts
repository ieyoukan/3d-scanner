import { describe, expect, it } from 'vitest';
import { thinPairs } from './liveVideo';

describe('thinPairs', () => {
  const items = (values: number[]) => values.map((sharpness, i) => ({ id: i, sharpness }));

  it('2枚ずつ組にして、くっきりした方を残す（並び順は保つ）', () => {
    const { kept, dropped } = thinPairs(items([5, 9, 7, 3, 1, 2]));
    expect(kept.map((x) => x.id)).toEqual([1, 2, 5]);
    expect(dropped.map((x) => x.id)).toEqual([0, 3, 4]);
  });

  it('奇数枚なら最後の1枚は残す', () => {
    const { kept, dropped } = thinPairs(items([5, 9, 7]));
    expect(kept.map((x) => x.id)).toEqual([1, 2]);
    expect(dropped.map((x) => x.id)).toEqual([0]);
  });

  it('同じくっきり度なら前の方を残す', () => {
    expect(thinPairs(items([4, 4])).kept.map((x) => x.id)).toEqual([0]);
  });
});
