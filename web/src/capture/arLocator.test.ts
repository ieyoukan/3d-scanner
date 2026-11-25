import * as THREE from 'three';
import { describe, expect, it, vi } from 'vitest';
import type { LocalizeResult, SceneInfo } from '../api/types';
import type { Grabber } from './arCamera';
import { createLocator } from './arLocator';

// 画像を JPEG にする処理はブラウザの canvas を使うので、テストでは中身の無い写真にする。
vi.mock('./frames', () => ({
  frameFromVideoFrame: async () => ({ blob: new Blob(['jpeg']) }),
  releaseFrame: () => {},
}));

const info = (extra: Partial<SceneInfo> = {}): SceneInfo => ({
  totalImages: 1,
  registeredImages: 1,
  points: 1,
  meanReprojectionError: 0,
  up: [0, 1, 0],
  center: [0, 0, 0],
  radius: 1,
  cameraPositions: [[0, 0, 2]],
  ...extra,
});

const grab = { snapshot: async () => ({ close() {} }) } as unknown as Grabber;
const image = { tex: new THREE.Texture(), width: 640, height: 480 };
const fwd = new THREE.Vector3(0, 0, -1);
const at = (x: number) => ({ pos: new THREE.Vector3(x, 1.5, 0), fwd });
const flush = () => new Promise((r) => setTimeout(r, 0));

/** 地図＝AR（変換なし）として、AR の位置をそのまま地図の位置として返す偽の位置合わせ。 */
function fakeLocalize(found = true) {
  const calls: number[] = [];
  let x = 0;
  return {
    calls,
    setX: (v: number) => (x = v),
    localize: async (): Promise<LocalizeResult> => {
      calls.push(x);
      return {
        localized: found,
        position: found ? [x, 1.5, 0] : undefined,
        forward: found ? [0, 0, -1] : undefined,
        matches: 100,
        comparedImages: 10,
        mapImages: 10,
        durationMs: 30000,
        message: '',
      };
    },
  };
}

describe('createLocator', () => {
  it('周りを覚える: 動いた距離でメーターが進み、25cm で照合に移る。手の揺れは数えない', () => {
    const loc = createLocator({ info: info({ metersPerUnit: 1 }), localize: fakeLocalize().localize, grab });
    loc.update(0, at(0), null);
    for (let i = 0; i < 100; i++) loc.update(i, at(i % 2 ? 0.001 : 0), null); // 1mm の揺れ
    expect(loc.progress()).toMatchObject({ stage: 'warmup', amount: 0 });
    loc.update(1, at(0.1), null);
    expect(loc.progress().amount).toBeCloseTo(0.4);
    loc.update(2, at(0.26), null);
    expect(loc.progress().stage).toBe('checking');
  });

  it('位置を見失っている間は進めない', () => {
    const loc = createLocator({ info: info(), localize: fakeLocalize().localize, grab });
    loc.update(0, at(0), null);
    loc.update(1, null, null);
    loc.update(2, at(1), null); // 見失った後に飛んだ位置は数えない
    expect(loc.progress()).toMatchObject({ stage: 'warmup', lost: false, amount: 0 });
    loc.update(3, null, null);
    expect(loc.progress().lost).toBe(true);
  });

  it('大きさが分かっていれば、照合 1 回で重なる', async () => {
    const fake = fakeLocalize();
    const loc = createLocator({ info: info({ metersPerUnit: 1 }), localize: fake.localize, grab, warmupMeters: 0.1 });
    loc.update(0, at(0), image);
    loc.update(1, at(0.2), image); // 覚えた
    loc.update(2, at(0.2), image); // 送る
    expect(loc.progress().stage).toBe('checking');
    await flush();
    await flush();
    expect(fake.calls.length).toBe(1);
    expect(loc.progress()).toMatchObject({ stage: 'aligned', found: 1, expectedSeconds: 30 });
    expect(loc.alignment()).not.toBeNull();
  });

  it('大きさが分からなければ、横へ 30cm 動いてもう 1 回照合する', async () => {
    const fake = fakeLocalize();
    const loc = createLocator({ info: info(), localize: fake.localize, grab, warmupMeters: 0.1 });
    loc.update(0, at(0), image);
    loc.update(1, at(0.2), image);
    fake.setX(0.2);
    loc.update(2, at(0.2), image);
    await flush();
    await flush();
    expect(loc.progress()).toMatchObject({ stage: 'move', found: 1, needFixes: 1 });
    loc.update(3, at(0.35), image); // まだ 15cm
    expect(loc.progress().amount).toBeCloseTo(0.5);
    expect(fake.calls.length).toBe(1);
    fake.setX(0.55);
    loc.update(4, at(0.55), image); // 35cm 離れた
    await flush();
    await flush();
    expect(fake.calls.length).toBe(2);
    expect(loc.progress().stage).toBe('aligned');
  });

  it('見つからなければ、少し待ってからもう一度', async () => {
    const fake = fakeLocalize(false);
    const loc = createLocator({ info: info({ metersPerUnit: 1 }), localize: fake.localize, grab, warmupMeters: 0.1 });
    loc.update(0, at(0), image);
    loc.update(1, at(0.2), image);
    loc.update(2, at(0.2), image);
    await flush();
    await flush();
    expect(loc.progress()).toMatchObject({ stage: 'checking', missed: 1, found: 0 });
    loc.update(3, at(0.2), image); // 1.5 秒たっていないので送らない
    expect(fake.calls.length).toBe(1);
  });
});
