import { describe, expect, it } from 'vitest';
import { buildMapFrame, fitView, headingOnMap, toMap, type Vec3 } from './mapView';

const near = (a: number[], b: number[]) => a.length === b.length && a.every((v, i) => Math.abs(v - (b[i] ?? NaN)) < 1e-9);

describe('buildMapFrame / toMap', () => {
  // 上が +Z、被写体が原点、最初の写真は手前（-Y 側）から撮った場面。
  const front: Vec3 = [0, -3, 1.5];
  const side: Vec3 = [3, 0, 1.5];
  const info = { up: [0, 0, 1] as Vec3, center: [0, 0, 0] as Vec3, cameraPositions: [front, side] };
  const frame = buildMapFrame(info);

  it('最初の写真の位置が中心の下（手前）に来る', () => {
    const [x, y] = toMap(frame, front);
    expect(x).toBeCloseTo(0);
    expect(y).toBeGreaterThan(0); // SVG は下が +y
    expect(y).toBeCloseTo(3);
  });

  it('真上から見下ろした向き（鏡写しにならない）', () => {
    // 手前から被写体を見て右（+X）にある写真は、地図でも右に来る。
    const [x, y] = toMap(frame, side);
    expect(x).toBeCloseTo(3);
    expect(y).toBeCloseTo(0);
  });

  it('高さ（up 方向）は地図に影響しない', () => {
    expect(near(toMap(frame, [1, 1, 0]), toMap(frame, [1, 1, 10]))).toBe(true);
  });

  it('カメラの向きを地図上の向きにする', () => {
    // 手前から奥（+Y）を向いたカメラは、地図の上（-y）を向く。
    const h = headingOnMap(frame, [0, 1, 0]);
    expect(h).not.toBeNull();
    expect(near(h!, [0, -1])).toBe(true);
    // 少し下を向いていても、床と平行な成分で向きを決める。
    const tilted = headingOnMap(frame, [0.6, 0, -0.8]);
    expect(near(tilted!, [1, 0])).toBe(true);
    // 真下を向いていると向きは描けない。
    expect(headingOnMap(frame, [0, 0, -1])).toBeNull();
  });

  it('写真が無くても、up に垂直な向きで地図を作れる', () => {
    const f = buildMapFrame({ up: [0, -1, 0], center: [1, 2, 3], cameraPositions: [] });
    const [x, y] = toMap(f, [1, 2, 3]);
    expect(x).toBeCloseTo(0);
    expect(y).toBeCloseTo(0);
    expect(Math.abs(f.right[1])).toBeLessThan(1e-9);
    expect(Math.abs(f.ahead[1])).toBeLessThan(1e-9);
  });
});

describe('fitView', () => {
  it('全部の点が余白つきで正方形に収まる', () => {
    const v = fitView(
      [
        [-2, 0],
        [4, 1],
      ],
      1,
    );
    expect(v.size).toBeCloseTo(6 * 1.3);
    expect(v.minX + v.size / 2).toBeCloseTo(1);
    expect(v.minY + v.size / 2).toBeCloseTo(0.5);
  });

  it('点が1つでも、最小の大きさを保つ', () => {
    const v = fitView([[5, 5]], 2);
    expect(v.size).toBeCloseTo(2 * 1.3);
    expect(fitView([], 2).size).toBe(2);
  });
});
