import * as THREE from 'three';
import { describe, expect, it } from 'vitest';
import { solveAlignment, type ArFix, type Vec3 } from './arAlign';

// 地図の座標: 真上が -z、1 が 0.25 m。AR の座標は、地図を「真上を +y に直し、鉛直軸で 40° 回し、
// 0.25 倍して (1, 0.5, -2) ずらした」もの。
const up: Vec3 = [0, 0, -1];
const truth = (() => {
  const tilt = new THREE.Quaternion().setFromUnitVectors(new THREE.Vector3(0, 0, -1), new THREE.Vector3(0, 1, 0));
  const q = new THREE.Quaternion().setFromAxisAngle(new THREE.Vector3(0, 1, 0), (40 * Math.PI) / 180).multiply(tilt);
  return new THREE.Matrix4().compose(new THREE.Vector3(1, 0.5, -2), q, new THREE.Vector3(0.25, 0.25, 0.25));
})();
const rotationOnly = new THREE.Matrix4().extractRotation(truth);

/** 地図でのカメラ（位置と向き）から、AR での同じカメラを作って fix にする。 */
function fix(mapPos: Vec3, mapFwd: Vec3): ArFix {
  const a = new THREE.Vector3(...mapPos).applyMatrix4(truth);
  const h = new THREE.Vector3(...mapFwd).applyMatrix4(rotationOnly).normalize();
  return { arPos: [a.x, a.y, a.z], arFwd: [h.x, h.y, h.z], mapPos, mapFwd };
}

const expectSame = (m: THREE.Matrix4, p: Vec3) => {
  const got = new THREE.Vector3(...p).applyMatrix4(m);
  const want = new THREE.Vector3(...p).applyMatrix4(truth);
  expect(got.distanceTo(want)).toBeLessThan(1e-6);
};

describe('solveAlignment', () => {
  it('大きさが分かっていれば、位置合わせ1回で重ねられる', () => {
    const r = solveAlignment([fix([4, 1, -2], [-1, 0, 0.3])], up, 0.25);
    expect(r.kind).toBe('aligned');
    if (r.kind !== 'aligned') return;
    expectSame(r.matrix, [0, 0, 0]);
    expectSame(r.matrix, [1, 2, -3]);
  });

  it('大きさが分からなければ、離れた2か所で求める', () => {
    const first = fix([4, 1, -2], [-1, 0, 0.3]);
    expect(solveAlignment([first], up).kind).toBe('need-baseline');
    // 地図で 0.4 しか離れていない（AR で 10cm）所では、まだ求めない。
    expect(solveAlignment([first, fix([4, 1.4, -2], [-1, 0, 0.3])], up).kind).toBe('need-baseline');
    const r = solveAlignment([first, fix([1, 4, -2], [0, -1, 0.3])], up);
    expect(r.kind).toBe('aligned');
    if (r.kind !== 'aligned') return;
    expect(r.scale).toBeCloseTo(0.25, 6);
    expectSame(r.matrix, [0, 0, 0]);
    expectSame(r.matrix, [2, -1, 0.5]);
  });

  it('レンズの向きのねじれ（写真の上下）には左右されない、真下を向いた fix は方位に使わない', () => {
    const down = fix([0, 0, -4], [0, 0, 1]); // 真上から真下を見ている
    expect(solveAlignment([down], up, 0.25).kind).toBe('none');
    const r = solveAlignment([down, fix([4, 1, -2], [-1, 0, 0.3])], up, 0.25);
    expect(r.kind).toBe('aligned');
  });
});
