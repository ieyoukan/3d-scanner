// AR で見る: 出来上がったメッシュを、撮った場所で実物に重ねて見比べる（スマホの AR、WebXR / ARCore）。
//
// 流れ:
//   ready     カメラの映像が見えている。「重ねる」を押すと、その瞬間のカメラ画像を位置合わせ（VPS）に送る
//   checking  照合中。経過秒数と目安（前回かかった秒数）をメーターで見せる
//   overlay   重なった。メッシュを、写真を撮った瞬間のカメラから見た形でぴったり重ねる。透かして実物と見比べられる
//   failed    見つからなかった・断られた。理由とやり直し方を見せる
//
// なぜ AR の「位置」を使わないのか: 実機の記録では、AR（ARCore）の位置（移動）は 40 秒の撮影で 18m 以上飛ぶほど
// 狂っていた（その場で 1m 以上ずれる）。一方、向き（回転）は写真から求めた向きと 1.5° 以内で合っていた。
// そこで、重ねる瞬間の写真 1 枚の位置合わせで「地図でのカメラの位置と向き」を求め、その瞬間の AR の向きとそろえる
// （resume/arAlign.ts の snapshotAlignment）。以後は AR の向きだけで追い、AR の位置の変化は打ち消す（メッシュを
// カメラの位置に対して固定する）。その場で見回す分には合ったまま。歩いて場所を変えたら「重ね直す」。
//
// 物を囲んで撮ったスキャンは、撮ったカメラの輪の内側だけを残す（背景の膜が画面を覆わないように）。
import * as THREE from 'three';
import { GLTFLoader, type GLTF } from 'three/addons/loaders/GLTFLoader.js';
import type { LocalizeResult, SceneInfo } from '../api/types';
import { cameraImageOf, makeGrabber } from '../capture/arCamera';
import { frameFromVideoFrame, releaseFrame } from '../capture/frames';
import { snapshotAlignment } from '../resume/arAlign';
import { cameraDistance, cropMesh, keepFraction } from './arModel';
import { unlit } from './materials';

export type ArViewPhase = 'loading' | 'ready' | 'checking' | 'overlay' | 'failed';

export interface ArViewState {
  phase: ArViewPhase;
  /** 照合を始めてからの秒数と、目安の秒数（前回かかった時間）。 */
  checkingSeconds: number;
  expectedSeconds: number;
  /** 重ねた結果（一致した特徴点の数）。 */
  matches: number | null;
  /** 見つからなかった・失敗したときの説明。 */
  failure: string | null;
  /** メッシュを透かして実物と見比べている。 */
  seeThrough: boolean;
  /** メッシュを読み込めなかった・カメラ画像を受け取れない等、続けられない問題。 */
  error: string | null;
}

export interface ArViewController {
  /** いまのカメラ画像で重ねる（重ねた後なら重ね直す）。 */
  align(): void;
  /** メッシュを透かす・戻す。 */
  toggleSeeThrough(): void;
  /** AR を閉じる。 */
  end(): void;
}

const stateIntervalMs = 100;
/** 透かすときの不透明度。 */
const seeThroughOpacity = 0.45;
/** 照合に送る画像の長辺（画素）。サーバーの特徴抽出も 1600px に縮めるので、それ以上は送らない。 */
const queryEdge = 1600;
/** 前回の照合にかかった秒数を覚えておく場所（目安に使う）。 */
const expectedKey = 'ar.localizeSeconds';

/**
 * AR で見始める。利用者の操作（タップ）の中で、ほかの await より先に呼ぶこと。
 * @param localize 位置合わせ（スキャンの地図に対して写真 1 枚の位置を求める）
 */
export async function startArView(opts: {
  overlay: HTMLElement;
  meshUrl: string;
  info: SceneInfo;
  /** 風景モードで撮ったスキャン（切り抜かない）。 */
  scene?: boolean;
  localize: (image: Blob, signal: AbortSignal) => Promise<LocalizeResult>;
  onState: (s: ArViewState) => void;
  onEnded: () => void;
}): Promise<ArViewController> {
  const xr = navigator.xr;
  if (!xr) throw new Error('この端末では AR を使えません。');
  const sessionPromise = xr.requestSession('immersive-ar', {
    // camera-access: 重ねる瞬間のカメラ画像を位置合わせに送るのに要る。
    requiredFeatures: ['local', 'camera-access', 'dom-overlay'],
    domOverlay: { root: opts.overlay },
  } as XRSessionInit);
  const gltfPromise = new GLTFLoader().loadAsync(opts.meshUrl);
  gltfPromise.catch(() => {}); // 失敗は下で扱う

  const renderer = new THREE.WebGLRenderer({ alpha: true, antialias: true });
  renderer.setPixelRatio(window.devicePixelRatio);
  renderer.xr.enabled = true;
  renderer.xr.setReferenceSpaceType('local');

  let session: XRSession;
  try {
    session = await sessionPromise;
  } catch (err) {
    renderer.dispose();
    throw new Error(
      err instanceof DOMException && err.name === 'NotSupportedError'
        ? 'この端末では重ねられません（AR のカメラ画像を受け取れません）。'
        : 'AR を始められませんでした。',
    );
  }
  await renderer.xr.setSession(session);
  const refSpace = renderer.xr.getReferenceSpace()!;
  const grab = makeGrabber(renderer);

  // ---- 3D の表示 ----
  //   follow  重ねた瞬間からの AR のカメラの位置の変化を打ち消す（メッシュをカメラの位置に対して固定する）
  //   inner   地図の座標 → AR の座標（snapshotAlignment）
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera();
  const follow = new THREE.Group();
  const inner = new THREE.Group();
  inner.matrixAutoUpdate = false;
  follow.add(inner);
  follow.visible = false;
  scene.add(follow);

  let phase: ArViewPhase = 'loading';
  let seeThrough = false;
  let error: string | null = null;
  let failure: string | null = null;
  let matches: number | null = null;
  let ended = false;
  let lastState = 0;
  let model: GLTF | null = null;
  let alignRequested = false;
  let checkStart = 0;
  let now = 0;
  let abort: AbortController | null = null;
  /** 重ねた瞬間の AR のカメラの位置（以後の位置の変化を打ち消す基準）。 */
  const alignedAt = new THREE.Vector3();
  const materials: THREE.MeshBasicMaterial[] = [];
  let expectedSeconds = readExpected();

  // 縮尺: 実物大が分かっていればそれ。無ければ「撮ったカメラから被写体まで 1m」とする（見た目には影響しない。
  // 小さすぎて描画の手前の限界（近くの面が切れる）に掛からないための値）。
  const orbit = cameraDistance(opts.info) ?? (opts.info.radius > 0 ? opts.info.radius : 1);
  const scale = opts.info.metersPerUnit && opts.info.metersPerUnit > 0 ? opts.info.metersPerUnit : 1 / orbit;

  const emit = (force = false) => {
    const t = performance.now();
    if (!force && t - lastState < stateIntervalMs) return;
    lastState = t;
    opts.onState({
      phase,
      checkingSeconds: phase === 'checking' ? Math.max(0, (now - checkStart) / 1000) : 0,
      expectedSeconds,
      matches,
      failure,
      seeThrough,
      error,
    });
  };

  gltfPromise
    .then((gltf) => {
      if (ended) return;
      gltf.scene.traverse((obj) => {
        const mesh = obj as THREE.Mesh;
        if (!mesh.isMesh) return;
        const m = unlit(mesh.material) as THREE.MeshBasicMaterial;
        mesh.material = m;
        materials.push(m);
      });
      // 物を囲んで撮ったスキャンは、撮ったカメラの輪の内側だけを残す。
      if (!opts.scene) cropMesh(gltf.scene, new THREE.Vector3(...opts.info.center), keepFraction * orbit);
      inner.add(gltf.scene);
      model = gltf;
      phase = 'ready';
      emit(true);
    })
    .catch(() => {
      error = 'メッシュを読み込めませんでした。';
      emit(true);
    });

  /** いまのフレームのカメラ画像と AR の位置・向きで、位置合わせに送る。 */
  function sendSnapshot(view: XRView) {
    const image = cameraImageOf(renderer, view);
    if (!image) {
      failure = 'カメラの画像を受け取れませんでした。もう一度押してください。';
      phase = 'failed';
      emit(true);
      return;
    }
    const t = view.transform;
    const arPos: [number, number, number] = [t.position.x, t.position.y, t.position.z];
    const arQuat: [number, number, number, number] = [t.orientation.x, t.orientation.y, t.orientation.z, t.orientation.w];
    phase = 'checking';
    checkStart = now;
    failure = null;
    emit(true);
    const controller = new AbortController();
    abort = controller;
    void (async () => {
      let bitmap: ImageBitmap | undefined;
      try {
        bitmap = await grab.snapshot(image.tex, image.width, image.height, queryEdge);
        const frame = await frameFromVideoFrame(bitmap, new Date().toISOString());
        releaseFrame(frame);
        const result = await opts.localize(frame.blob, controller.signal);
        if (ended || controller.signal.aborted) return;
        if (result.durationMs > 0) {
          expectedSeconds = Math.round(result.durationMs / 1000);
          writeExpected(expectedSeconds);
        }
        if (!result.localized || !result.position || !result.forward || !result.down) {
          matches = null;
          failure = '見つかりませんでした。撮ったときと同じ所から、撮ったものが大きく写るように向けて、もう一度押してください。';
          phase = 'failed';
          emit(true);
          return;
        }
        inner.matrix.copy(
          snapshotAlignment(
            { position: result.position, forward: result.forward, down: result.down },
            { position: arPos, orientation: arQuat },
            scale,
          ),
        );
        alignedAt.set(...arPos);
        matches = result.matches;
        follow.visible = true;
        phase = 'overlay';
        emit(true);
      } catch (err) {
        if (ended || controller.signal.aborted) return;
        failure =
          err instanceof Error && /処理中|busy/.test(err.message)
            ? '別の位置合わせを処理中です。少し待ってからもう一度押してください。'
            : '照合できませんでした（通信やサーバーの問題）。もう一度押してください。';
        phase = 'failed';
        emit(true);
      } finally {
        bitmap?.close();
        if (abort === controller) abort = null;
      }
    })();
  }

  session.addEventListener('end', () => {
    ended = true;
    abort?.abort();
    renderer.setAnimationLoop(null);
    grab.dispose();
    model?.scene.traverse((obj) => {
      const mesh = obj as THREE.Mesh;
      if (!mesh.isMesh) return;
      mesh.geometry.dispose();
      const m = mesh.material as THREE.MeshBasicMaterial;
      m.map?.dispose();
      m.dispose();
    });
    renderer.dispose();
    opts.onEnded();
  });

  renderer.setAnimationLoop((t: number, frame?: XRFrame) => {
    if (!frame) return;
    now = t;
    const pose = frame.getViewerPose(refSpace);
    if (pose) {
      const view = pose.views[0]!;
      if (alignRequested) {
        alignRequested = false;
        sendSnapshot(view);
      }
      // 重ねた瞬間からの AR の位置の変化を打ち消す（AR の位置は狂いやすいので、向きだけで追う）。
      const p = view.transform.position;
      follow.position.set(p.x - alignedAt.x, p.y - alignedAt.y, p.z - alignedAt.z);
    }
    renderer.render(scene, camera);
    emit();
  });

  emit(true);

  return {
    align() {
      if (phase === 'loading' || phase === 'checking' || error) return;
      follow.visible = false;
      // カメラ画像はフレームの中でしか取れないので、次のフレームで送る。
      alignRequested = true;
    },
    toggleSeeThrough() {
      seeThrough = !seeThrough;
      for (const m of materials) {
        m.transparent = seeThrough;
        m.opacity = seeThrough ? seeThroughOpacity : 1;
        m.depthWrite = !seeThrough;
        m.needsUpdate = true;
      }
      emit(true);
    },
    end() {
      if (!ended) void session.end().catch(() => {});
    },
  };
}

function readExpected(): number {
  try {
    const v = Number(window.localStorage.getItem(expectedKey));
    return v > 0 ? v : 40;
  } catch {
    return 40;
  }
}

function writeExpected(seconds: number) {
  try {
    window.localStorage.setItem(expectedKey, String(seconds));
  } catch {
    // 覚えられなくても困らない（次も既定の目安を使う）。
  }
}
