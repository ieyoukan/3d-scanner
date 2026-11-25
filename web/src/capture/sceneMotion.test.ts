import { describe, expect, it } from 'vitest';
import { MotionMeter, MotionWarnings, type Vec3 } from './sceneMotion';

const yaw = (d: number): Vec3 => [Math.sin((d * Math.PI) / 180), 0, -Math.cos((d * Math.PI) / 180)];

describe('MotionMeter', () => {
  it('15cm 動くか 12° 向きを変えるごとに、区間が 1 進む', () => {
    const m = new MotionMeter();
    expect(m.update([0, 0, 0], yaw(0), 1)).toBe(0);
    expect(m.update([0.075, 0, 0], yaw(0), 1)).toBeCloseTo(0.5);
    expect(Math.floor(m.update([0.16, 0, 0], yaw(0), 1))).toBe(1);
    // 向きだけ 12° 変える。
    expect(Math.floor(m.update([0.16, 0, 0], yaw(13), 1))).toBe(2);
  });

  it('立ち止まって手が揺れているだけなら、区間は進まない', () => {
    const m = new MotionMeter();
    m.update([0, 0, 0], yaw(0), 1);
    let c = 0;
    for (let i = 0; i < 600; i++) {
      c = m.update([0.004 * Math.sin(i), 0.003 * Math.cos(i), 0], yaw(Math.sin(i)), 1);
    }
    expect(c).toBeLessThan(1);
  });

  it('区間を倍にしたら、倍動くまで進まない（区間の番号は 座標 ÷ 区間の長さ）', () => {
    const m = new MotionMeter();
    m.update([0, 0, 0], yaw(0), 2);
    expect(Math.floor(m.update([0.2, 0, 0], yaw(0), 2) / 2)).toBe(0);
    expect(m.update([0.31, 0, 0], yaw(0), 2) / 2).toBe(1);
  });
});

describe('MotionWarnings', () => {
  it('その場で向きだけ変えると spin', () => {
    const w = new MotionWarnings();
    for (let t = 0; t <= 2000; t += 100) w.push(t, [0, 1.5, 0], yaw(t / 25)); // 秒速 40°、位置は同じ
    expect(w.warning(2000)).toBe('spin');
  });

  it('横に歩きながら向きを変えるのは警告しない', () => {
    const w = new MotionWarnings();
    for (let t = 0; t <= 2000; t += 100) w.push(t, [t / 4000, 1.5, 0], yaw(t / 25)); // 秒速 25cm、秒速 40°
    expect(w.warning(2000)).toBeNull();
  });

  it('速すぎると fast。止まっても 1 秒は出し続ける', () => {
    const w = new MotionWarnings();
    for (let t = 0; t <= 1000; t += 100) w.push(t, [t / 1000, 1.5, 0], yaw(0)); // 秒速 1m
    expect(w.warning(1000)).toBe('fast');
    for (let t = 1100; t <= 1800; t += 100) w.push(t, [1, 1.5, 0], yaw(0));
    expect(w.warning(1800)).toBe('fast');
    for (let t = 1900; t <= 2600; t += 100) w.push(t, [1, 1.5, 0], yaw(0));
    expect(w.warning(2600)).toBeNull();
  });
});
