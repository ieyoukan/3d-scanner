// AR で、前回の3Dデータ（地図）を実物に重ねるための位置合わせ。「AR で見る」と「続きを撮る」で共通。
//
// 段階（画面のメーターに出す。どれも本当に測った量で進む）:
//   warmup   端末を動かして、AR（ARCore）に周りを覚えさせる。動いた距離が warmupMeters になるまで。
//            AR は動き始めの数秒、自分の位置の推定を大きく直す。その間に照合すると、重ねた後にずれる。
//   checking カメラ画像をサーバーの位置合わせ（VPS）に送って待つ。経過秒数と、前回かかった秒数（目安）を出す。
//            見つからなければ 1.5 秒後、エラーなら 4 秒後にもう一度。
//   move     大きさが分からないスキャン（AR で撮っていない）は、離れたもう 1 か所でも照合して大きさを求める。
//            前に照合した所から minBaselineMeters 離れるまで、動いた距離で進む。
//   aligned  重なった（地図の座標 → AR の座標の変換が決まった）。
// 位置を見失っている間（AR が向きしか分からない）は、どの段階でも進めない（lost）。
import * as THREE from 'three';
import type { LocalizeResult, SceneInfo } from '../api/types';
import { minBaselineMeters, solveAlignment, type ArFix, type Vec3 } from '../resume/arAlign';
import type { Grabber } from './arCamera';
import { frameFromVideoFrame, releaseFrame } from './frames';

export type LocateStage = 'warmup' | 'checking' | 'move' | 'aligned';

export interface LocateProgress {
  stage: LocateStage;
  /** その段階のメーター（0〜1）。checking は経過秒数 ÷ 目安（1 を超えたら 1）。 */
  amount: number;
  /** 位置を見失っている（端末を速く動かした・暗い・何も無い所を写している）。 */
  lost: boolean;
  /** 照合を始めてからの秒数と、目安の秒数（前回かかった時間）。 */
  checkingSeconds: number;
  expectedSeconds: number;
  /** 位置が分かった回数・分からなかった回数。 */
  found: number;
  missed: number;
  /** 大きさを求めるのに、照合があと何か所要るか（0 なら不要）。 */
  needFixes: number;
}

export interface LocatorOptions {
  info: SceneInfo;
  localize: (image: Blob, signal: AbortSignal) => Promise<LocalizeResult>;
  grab: Grabber;
  /** 周りを覚えるのに動いてほしい距離（メートル）。 */
  warmupMeters?: number;
}

/** 照合に送る画像の長辺（画素）。サーバーの特徴抽出も 1600px に縮めるので、それ以上は送らない。 */
const queryEdge = 1600;
const retryMs = 1500;
const errorRetryMs = 4000;
/** 1 フレームでこれ以下しか動いていなければ、手の揺れとみなして数えない（メートル）。 */
const jitterMeters = 0.002;

export function createLocator(opts: LocatorOptions) {
  const warmupMeters = opts.warmupMeters ?? 0.25;
  const fixes: ArFix[] = [];
  let stage: LocateStage = 'warmup';
  let travelled = 0;
  let last: THREE.Vector3 | null = null;
  let lost = true;
  let checking = false;
  let checkStart = 0;
  let now = 0;
  let expectedSeconds = 45;
  let nextAt = 0;
  let found = 0;
  let missed = 0;
  let matrix: THREE.Matrix4 | null = null;
  let abort: AbortController | null = null;
  let disposed = false;
  const pos = new THREE.Vector3();

  const distanceFromFixes = () =>
    fixes.length === 0 ? 0 : Math.min(...fixes.map((f) => pos.distanceTo(new THREE.Vector3(...f.arPos))));

  /** いまのカメラ画像を照合に送る。そのときの AR の位置と向きを控え、結果と組にする。 */
  function send(image: { tex: THREE.Texture; width: number; height: number }, camPos: THREE.Vector3, camFwd: THREE.Vector3) {
    checking = true;
    checkStart = now;
    nextAt = Infinity;
    const arPos: Vec3 = [camPos.x, camPos.y, camPos.z];
    const arFwd: Vec3 = [camFwd.x, camFwd.y, camFwd.z];
    const controller = new AbortController();
    abort = controller;
    const done = (retry: number) => {
      checking = false;
      abort = null;
      // 時刻は update で受け取った最新の値（XR のフレームの時刻）を基準にする。
      nextAt = now + retry;
    };
    void (async () => {
      let bitmap: ImageBitmap | undefined;
      try {
        bitmap = await opts.grab.snapshot(image.tex, image.width, image.height, queryEdge);
        const frame = await frameFromVideoFrame(bitmap, new Date().toISOString());
        releaseFrame(frame);
        const result = await opts.localize(frame.blob, controller.signal);
        if (disposed) return;
        if (result.durationMs > 0) expectedSeconds = Math.round(result.durationMs / 1000);
        if (!result.localized || !result.position || !result.forward) {
          missed += 1;
          done(retryMs);
          return;
        }
        found += 1;
        fixes.push({ arPos, arFwd, mapPos: result.position, mapFwd: result.forward });
        const solved = solveAlignment(fixes, opts.info.up, opts.info.metersPerUnit);
        if (solved.kind === 'aligned') {
          matrix = solved.matrix;
          stage = 'aligned';
        } else if (solved.kind === 'need-baseline') {
          stage = 'move';
        }
        done(0);
      } catch {
        if (disposed || controller.signal.aborted) return;
        missed += 1;
        done(errorRetryMs);
      } finally {
        bitmap?.close();
      }
    })();
  }

  return {
    /**
     * 毎フレーム呼ぶ。
     * @param t       時刻（ミリ秒）
     * @param camera  カメラの位置と視線。位置を見失っているときは null
     * @param image   そのフレームのカメラ画像（camera-access が無ければ null）
     */
    update(
      t: number,
      camera: { pos: THREE.Vector3; fwd: THREE.Vector3 } | null,
      image: { tex: THREE.Texture; width: number; height: number } | null,
    ) {
      now = t;
      lost = camera === null;
      if (!camera || stage === 'aligned') {
        last = null;
        return;
      }
      pos.copy(camera.pos);
      if (stage === 'warmup') {
        if (last) {
          const d = last.distanceTo(pos);
          if (d > jitterMeters) travelled += d;
        }
        last = (last ?? new THREE.Vector3()).copy(pos);
        if (travelled >= warmupMeters) stage = 'checking';
        return;
      }
      if (checking || t < nextAt || !image) return;
      if (stage === 'move') {
        if (distanceFromFixes() < minBaselineMeters) return;
        stage = 'checking';
      }
      send(image, camera.pos, camera.fwd);
    },

    progress(): LocateProgress {
      const checkingSeconds = checking ? Math.max(0, (now - checkStart) / 1000) : 0;
      let amount = 0;
      switch (stage) {
        case 'warmup':
          amount = Math.min(1, travelled / warmupMeters);
          break;
        case 'checking':
          amount = Math.min(1, checkingSeconds / Math.max(1, expectedSeconds));
          break;
        case 'move':
          amount = Math.min(1, distanceFromFixes() / minBaselineMeters);
          break;
        case 'aligned':
          amount = 1;
      }
      const needFixes = opts.info.metersPerUnit ? Math.max(0, 1 - found) : Math.max(0, 2 - found);
      return { stage, amount, lost, checkingSeconds, expectedSeconds, found, missed, needFixes };
    },

    /** 地図の座標 → AR の座標の変換（重なるまでは null）。 */
    alignment(): THREE.Matrix4 | null {
      return matrix;
    },

    /** 最初からやり直す（重ね直したいとき）。 */
    reset() {
      abort?.abort();
      fixes.length = 0;
      stage = 'warmup';
      travelled = 0;
      last = null;
      checking = false;
      matrix = null;
      nextAt = 0;
    },

    dispose() {
      disposed = true;
      abort?.abort();
    },
  };
}

export type Locator = ReturnType<typeof createLocator>;
