import * as THREE from 'three';
import { describe, expect, it } from 'vitest';
import type { SceneInfo } from '../api/types';
import { defaultRadiusMeters, placeModel } from './arModel';

// SfM の座標で、真上が -z、中心 (1, 2, 3)、最初のカメラは +x 側（中心から 4 離れた所）。
const info = (extra: Partial<SceneInfo> = {}): SceneInfo => ({
  totalImages: 1,
  registeredImages: 1,
  points: 1,
  meanReprojectionError: 0,
  up: [0, 0, -1],
  center: [1, 2, 3],
  radius: 0.5,
  cameraPositions: [[5, 2, 3]],
  ...extra,
});

// 中心の真下 0.2（-z 方向が上なので、z が +0.2）に底がある、縦の棒（高さ 0.6）。
const pole = Array.from({ length: 61 }, (_, i) => new THREE.Vector3(1, 2, 3.2 - i * 0.01));

const apply = (m: THREE.Matrix4, p: [number, number, number]) => new THREE.Vector3(...p).applyMatrix4(m);

describe('placeModel', () => {
  it('実物大: 真上を +y、底を y = 0、最初のカメラの側を +z にする', () => {
    const { matrix, realScale } = placeModel(info({ metersPerUnit: 0.5 }), pole);
    expect(realScale).toBe(true);
    // 底（z = 3.2）はほぼ y = 0（下から 2% の頂点を底にするので、頂点 1 つ分 = 0.5 cm 以内）、
    // 上端（z = 2.6）は底（下から2番目の頂点 z = 3.19）から 0.59 × 0.5 = 0.295 m。
    expect(Math.abs(apply(matrix, [1, 2, 3.2]).y)).toBeLessThanOrEqual(0.005 + 1e-9);
    expect(apply(matrix, [1, 2, 2.6]).y).toBeCloseTo(0.295, 6);
    // 中心は原点の真上。
    const c = apply(matrix, [1, 2, 3]);
    expect(c.x).toBeCloseTo(0, 6);
    expect(c.z).toBeCloseTo(0, 6);
    // 最初のカメラは +z 側、2 m 先（4 × 0.5）。
    const cam = apply(matrix, [5, 2, 3]);
    expect(cam.x).toBeCloseTo(0, 6);
    expect(cam.z).toBeCloseTo(2, 6);
  });

  it('大きさが分からなければ、半径が目安の大きさになる', () => {
    const { matrix, realScale } = placeModel(info(), pole);
    expect(realScale).toBe(false);
    const cam = apply(matrix, [5, 2, 3]);
    expect(cam.z).toBeCloseTo((4 * defaultRadiusMeters) / 0.5, 6);
  });

  it('遠くの床は底に含めない', () => {
    const floor = Array.from({ length: 200 }, (_, i) => new THREE.Vector3(1 + 5 + i * 0.01, 2, 3.9));
    const { matrix } = placeModel(info({ metersPerUnit: 1 }), [...pole, ...floor]);
    // 床（底より 0.7 下）に引っ張られず、棒の底がほぼ y = 0（頂点 1 つ分 = 1 cm 以内）。
    expect(Math.abs(apply(matrix, [1, 2, 3.2]).y)).toBeLessThanOrEqual(0.01 + 1e-9);
  });
});
