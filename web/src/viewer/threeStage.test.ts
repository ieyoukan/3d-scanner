import { describe, expect, it } from 'vitest';
import { baseFov, verticalFov } from './threeStage';

describe('verticalFov', () => {
  it('横長・正方形の画面では基準の画角のまま', () => {
    expect(verticalFov(16 / 9)).toBe(baseFov);
    expect(verticalFov(1)).toBe(baseFov);
  });

  it('縦長の画面では、横方向が基準の画角になるよう縦方向を広げる', () => {
    const aspect = 390 / 660; // スマホ縦持ちの表示領域
    const vertical = verticalFov(aspect);
    expect(vertical).toBeGreaterThan(baseFov);
    // 縦の画角から横の画角を計算し直すと、基準の値に戻る。
    const horizontal = (2 * Math.atan(Math.tan((vertical * Math.PI) / 360) * aspect) * 180) / Math.PI;
    expect(horizontal).toBeCloseTo(baseFov, 5);
  });

  it('極端に細長い画面でも、歪みが大きくなりすぎない上限で止まる', () => {
    expect(verticalFov(0.2)).toBe(85);
  });
});
