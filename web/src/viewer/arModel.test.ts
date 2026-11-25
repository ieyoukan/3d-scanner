import * as THREE from 'three';
import { describe, expect, it } from 'vitest';
import type { SceneInfo } from '../api/types';
import { assumedCameraMeters, cropMesh, keepFraction, placeModel, sceneModelRadiusMeters } from './arModel';

// SfM の座標で、真上が -z、中心 (1, 2, 3)、最初のカメラは +x 側（中心から 4 離れた所）。
// radius（点の 75% が入る半径）は背景に引っ張られて 10 と大きい。
const info = (extra: Partial<SceneInfo> = {}): SceneInfo => ({
  totalImages: 1,
  registeredImages: 1,
  points: 1,
  meanReprojectionError: 0,
  up: [0, 0, -1],
  center: [1, 2, 3],
  radius: 10,
  cameraPositions: [[5, 2, 3]],
  ...extra,
});

// 中心の真下 0.2（-z 方向が上なので、z が +0.2）に底がある、縦の棒（高さ 0.6）。
const pole = Array.from({ length: 61 }, (_, i) => new THREE.Vector3(1, 2, 3.2 - i * 0.01));
// カメラより外の背景（床）。中心から 5 以上離れていて、底より 0.7 下。
const floor = Array.from({ length: 200 }, (_, i) => new THREE.Vector3(6 + i * 0.01, 2, 3.9));

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

  it('大きさが分からなければ、撮ったカメラが 60cm 先にいた大きさにする（背景に引っ張られた radius は使わない）', () => {
    const { matrix, realScale } = placeModel(info(), pole);
    expect(realScale).toBe(false);
    expect(apply(matrix, [5, 2, 3]).z).toBeCloseTo(assumedCameraMeters, 6);
  });

  it('カメラの輪の内側だけを残す。外の背景は底にも使わない', () => {
    const { matrix, keepWithin } = placeModel(info({ metersPerUnit: 1 }), [...pole, ...floor]);
    expect(keepWithin).toBeCloseTo(keepFraction * 4, 6);
    expect(Math.abs(apply(matrix, [1, 2, 3.2]).y)).toBeLessThanOrEqual(0.01 + 1e-9);
  });

  it('風景モードは切り抜かず、全体を机の上の模型の大きさにする', () => {
    const { matrix, keepWithin } = placeModel(info(), [...pole, ...floor], true);
    expect(keepWithin).toBeNull();
    // 頂点の 90% が入る半径（床の遠い方）が 0.3 m 前後になる。
    const far = apply(matrix, [7.5, 2, 3.9]);
    const c = apply(matrix, [1, 2, 3]);
    expect(Math.hypot(far.x - c.x, far.z - c.z)).toBeLessThanOrEqual(sceneModelRadiusMeters * 1.05);
  });
});

describe('cropMesh', () => {
  it('中心から遠い頂点を含む三角形を消す', () => {
    const geometry = new THREE.BufferGeometry();
    geometry.setAttribute(
      'position',
      new THREE.Float32BufferAttribute([0, 0, 0, 1, 0, 0, 0, 1, 0, /* 遠い */ 50, 0, 0, 0, 0, 1], 3),
    );
    geometry.setIndex([0, 1, 2, 0, 3, 4, 1, 2, 4]);
    const root = new THREE.Group();
    const mesh = new THREE.Mesh(geometry);
    mesh.position.set(10, 0, 0); // メッシュ自身の変換も考える
    root.add(mesh);
    cropMesh(root, new THREE.Vector3(10, 0, 0), 2);
    expect(Array.from(geometry.index!.array)).toEqual([0, 1, 2, 1, 2, 4]);
  });
});
