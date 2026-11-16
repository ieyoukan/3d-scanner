import { describe, expect, it } from 'vitest';
import type { SceneInfo } from '../api/types';
import { computeViewFrame, rotate, rotationBetween, toView, type Vec3 } from './sceneFrame';

const close = (a: Vec3, b: Vec3) => {
  for (let i = 0; i < 3; i++) expect(a[i]).toBeCloseTo(b[i] as number, 6);
};

const info = (overrides: Partial<SceneInfo> = {}): SceneInfo => ({
  totalImages: 3,
  registeredImages: 3,
  points: 100,
  meanReprojectionError: 0.6,
  up: [0, -1, 0], // COLMAP でよくある向き（Y が下）
  center: [1, 2, 10],
  radius: 4,
  cameraPositions: [
    [1, 2, 0],
    [3, 2, 1],
  ],
  ...overrides,
});

describe('rotationBetween', () => {
  it.each<[Vec3, Vec3]>([
    [[0, -1, 0], [0, 1, 0]], // 真逆（特別扱いが必要な場合）
    [[1, 0, 0], [0, 1, 0]],
    [[0, 0, 1], [0, 1, 0]],
    [[0, 1, 0], [0, 1, 0]], // 既に同じ向き
    [[0.6, -0.8, 0], [0, 1, 0]],
    [[0.0217, -0.9993, 0.0288], [0, 1, 0]], // 実際の SfM 結果に近い値
  ])('%j を %j に重ねる', (from, to) => {
    const n = Math.hypot(...from);
    const unit: Vec3 = [from[0] / n, from[1] / n, from[2] / n];
    close(rotate(rotationBetween(unit, to), unit), to);
  });

  it('回転なので、ベクトルの長さは変わらない', () => {
    const q = rotationBetween([0, -1, 0], [0, 1, 0]);
    const v = rotate(q, [3, 4, 12]);
    expect(Math.hypot(...v)).toBeCloseTo(13, 6);
  });
});

describe('computeViewFrame', () => {
  it('被写体の中心が原点に、「上」が +Y に来る', () => {
    const frame = computeViewFrame(info());
    close(toView(frame, [1, 2, 10]), [0, 0, 0]);
    // 中心から「上」方向に 1 進んだ点は、ビューアでは (0, 1, 0)。
    close(toView(frame, [1, 1, 10]), [0, 1, 0]);
    expect(frame.approximate).toBe(false);
    expect(frame.radius).toBe(4);
  });

  it('カメラの初期位置は、実際に写真を撮った位置（1枚目）', () => {
    const frame = computeViewFrame(info());
    // 1枚目のカメラは中心から 10 離れている（radius 4 の 1.5〜8 倍の範囲内なので、そのまま使う）。
    close(frame.cameraPosition, toView(frame, [1, 2, 0]));
    expect(Math.hypot(...frame.cameraPosition)).toBeCloseTo(10, 6);
  });

  it('カメラが近すぎる・遠すぎるときは、見やすい距離に収める（方向は保つ）', () => {
    const near = computeViewFrame(info({ cameraPositions: [[1, 2, 9]] })); // 中心から 1
    expect(Math.hypot(...near.cameraPosition)).toBeCloseTo(4 * 1.5, 6);
    const far = computeViewFrame(info({ cameraPositions: [[1, 2, -990]] })); // 中心から 1000
    expect(Math.hypot(...far.cameraPosition)).toBeCloseTo(4 * 8, 6);
    // どちらも中心から見て同じ方向（同じ直線上）。
    const unit = (v: Vec3): Vec3 => {
      const n = Math.hypot(...v);
      return [v[0] / n, v[1] / n, v[2] / n];
    };
    close(unit(near.cameraPosition), unit(far.cameraPosition));
  });

  it('scene.json が無いときは仮の値を返し、そのことを伝える', () => {
    const frame = computeViewFrame(null);
    expect(frame.approximate).toBe(true);
    // 仮の回転でも、COLMAP の「Y が下」は「Y が上」になる。
    close(rotate(frame.rotation, [0, -1, 0]), [0, 1, 0]);
  });

  it('おかしな値（長さ 0 の up、0 以下の半径、カメラ無し）でも NaN を出さない', () => {
    const frame = computeViewFrame(info({ up: [0, 0, 0], radius: 0, cameraPositions: [] }));
    for (const v of [...frame.rotation, ...frame.position, ...frame.cameraPosition, frame.radius]) {
      expect(Number.isFinite(v)).toBe(true);
    }
    expect(frame.radius).toBeGreaterThan(0);
  });
});
