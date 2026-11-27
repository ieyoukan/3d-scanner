import * as THREE from 'three';
import { describe, expect, it } from 'vitest';
import type { SceneInfo } from '../api/types';
import { cameraDistance, cropMesh } from './arModel';

describe('cameraDistance', () => {
  it('撮ったカメラから中心までの距離の中央値（カメラが無ければ null）', () => {
    const info = { center: [0, 0, 0], cameraPositions: [[3, 0, 0], [0, 4, 0], [0, 0, 5]] } as unknown as SceneInfo;
    expect(cameraDistance(info)).toBe(4);
    expect(cameraDistance({ ...info, cameraPositions: [] })).toBeNull();
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
