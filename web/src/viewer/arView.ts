// AR で見る: 出来上がったメッシュを、スマホの AR（WebXR / ARCore）で見る。2 つの見方がある:
//
//   実物に重ねる（撮った場所で開いたとき）
//     locating  カメラ画像を位置合わせ（VPS）に送り、前回の3Dデータ（地図）を AR の座標に重ねる（capture/arLocator.ts）。
//               画面のメーターで「周りを覚える → 照合 →（横へ）」の進み具合を見せる
//     overlay   メッシュを実物大で、撮った物・部屋そのものに重ねる。透かして実物と見比べられる
//   机に置く（別の場所で開いたとき、位置合わせできないとき、利用者が選んだとき）
//     placing   照準（輪）が出る。タップした所にメッシュを置く（capture/arPlacement.ts）
//     placed    1本指で左右になぞると回る、2本指でつまむと大きさが変わる
//
// 位置合わせには、スキャンの地図（続きを撮るのと同じもの）と camera-access が要る。どちらかが無ければ机に置く。
//
// 置いた・重ねたメッシュはアンカー（capture/arAnchor.ts）で現実に貼り付ける。AR（ARCore）は動きながら自分の位置の
// 推定を直すので、貼り付けないと、その修正のたびにメッシュが流れて見える（実機で「置いた瞬間どこかへ逃げていく」）。
// 位置を見失っている間（AR が向きしか分からない）は、置かせず、メーターも進めない。
//
// 向き・大きさ（机に置くとき）は arModel.ts: 真上を合わせ、底を置いた面に乗せ、最初に撮った側を利用者に向ける。
// 物を囲んで撮ったスキャンは、撮ったカメラの輪の内側だけを残す（背景の膜を取り除く）。
import * as THREE from 'three';
import { GLTFLoader, type GLTF } from 'three/addons/loaders/GLTFLoader.js';
import type { LocalizeResult, SceneInfo } from '../api/types';
import { createAnchorKeeper } from '../capture/arAnchor';
import { cameraImageOf, makeGrabber } from '../capture/arCamera';
import { createLocator, type LocateProgress } from '../capture/arLocator';
import { createPlacer } from '../capture/arPlacement';
import { cropMesh, placeModel, sampleVertices } from './arModel';
import { unlit } from './materials';

export type ArViewPhase = 'loading' | 'locating' | 'overlay' | 'placing' | 'placed';

export interface ArViewState {
  phase: ArViewPhase;
  /** 端末の位置が分かっている（false なら AR が位置を見失っている）。 */
  tracking: boolean;
  /** 照準が出ている（タップで置ける）。 */
  aiming: boolean;
  /** 実物大で見られる（AR で撮ったスキャン）。机に置くとき。 */
  realScale: boolean;
  /** 実物大（または目安の大きさ）に対する倍率。机に置くとき。 */
  zoom: number;
  /** 実物に重ねられる（地図と camera-access がある）。 */
  canOverlay: boolean;
  /** 重ねるまでの進み具合（locating の間）。 */
  locate: LocateProgress | null;
  /** メッシュを透かして実物と見比べている。 */
  seeThrough: boolean;
  /** メッシュを読み込めなかった。 */
  error: string | null;
}

export interface ArViewController {
  /** 机に置く（重ねるのをやめて、照準で置く）。置いた後なら置き直す。 */
  place(): void;
  /** 実物に重ねる（位置合わせを最初から）。 */
  overlay(): void;
  /** 鉛直の軸のまわりに回す（ラジアン）。机に置いたとき。 */
  rotateBy(radians: number): void;
  /** 大きさを掛ける。机に置いたとき。 */
  zoomBy(factor: number): void;
  /** 元の大きさ（実物大）に戻す。 */
  resetZoom(): void;
  /** メッシュを透かす・戻す。 */
  toggleSeeThrough(): void;
  /** AR を閉じる。 */
  end(): void;
}

const minZoom = 0.1;
const maxZoom = 10;
const stateIntervalMs = 100;
/** 透かすときの不透明度。 */
const seeThroughOpacity = 0.45;
/** camera-access があるか分からない端末で、カメラ画像がこの時間来なければ机に置く方へ切り替える（ミリ秒）。 */
const noImageFallbackMs = 4000;

/**
 * AR で見始める。利用者の操作（タップ）の中で、ほかの await より先に呼ぶこと。
 * @param overlay  AR の上に重ねる画面の根の要素
 * @param localize 位置合わせ（スキャンに地図があるときだけ渡す。無ければ机に置く）
 */
export async function startArView(opts: {
  overlay: HTMLElement;
  meshUrl: string;
  info: SceneInfo;
  /** 風景モードで撮ったスキャン（机に置くときは切り抜かず、全体を模型の大きさにする）。 */
  scene?: boolean;
  localize?: (image: Blob, signal: AbortSignal) => Promise<LocalizeResult>;
  onState: (s: ArViewState) => void;
  onEnded: () => void;
}): Promise<ArViewController> {
  const xr = navigator.xr;
  if (!xr) throw new Error('この端末では AR を使えません。');
  const wantOverlay = !!opts.localize;
  const sessionPromise = xr.requestSession('immersive-ar', {
    requiredFeatures: ['local', 'hit-test', 'dom-overlay'],
    // camera-access: 実物に重ねるための位置合わせに使う。anchors: 置いた物を現実に貼り付ける。
    // どちらも無くても AR は始める（重ねられない・貼り付けない、だけ）。
    optionalFeatures: wantOverlay ? ['camera-access', 'anchors'] : ['anchors'],
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
  } catch {
    renderer.dispose();
    throw new Error('AR を始められませんでした。');
  }
  await renderer.xr.setSession(session);
  const refSpace = renderer.xr.getReferenceSpace()!;
  const placer = await createPlacer(session);
  const enabled = (session as XRSession & { enabledFeatures?: string[] }).enabledFeatures;
  // 対応しているか分からない（enabledFeatures が無い）ときは、とりあえず重ねにいき、画像が来なければ机に置く。
  let canOverlay = wantOverlay && (enabled ? enabled.includes('camera-access') : true);
  const grab = makeGrabber(renderer);
  const locator = canOverlay ? createLocator({ info: opts.info, localize: opts.localize!, grab }) : null;
  const keeper = createAnchorKeeper();

  // ---- 3D の表示 ----
  //   anchored  アンカーの補正（AR が位置の推定を直した分）
  //   pivot     机に置くとき: 置いた点・回した向き・倍率。重ねるときは単位行列
  //   inner     机に置くとき: モデルの座標 → 置いた点を原点とする座標。重ねるとき: 地図の座標 → AR の座標
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera();
  scene.add(placer.group);
  const anchored = new THREE.Group();
  anchored.matrixAutoUpdate = false;
  const pivot = new THREE.Group();
  const inner = new THREE.Group();
  inner.matrixAutoUpdate = false;
  pivot.add(inner);
  anchored.add(pivot);
  anchored.visible = false;
  scene.add(anchored);

  let phase: ArViewPhase = 'loading';
  let tracking = false;
  let realScale = false;
  let zoom = 1;
  let seeThrough = false;
  let error: string | null = null;
  let ended = false;
  let lastState = 0;
  let model: GLTF | null = null;
  let tablePlacement = new THREE.Matrix4();
  let noImageSince: number | null = null;
  const materials: THREE.MeshBasicMaterial[] = [];

  const emit = (force = false) => {
    const now = performance.now();
    if (!force && now - lastState < stateIntervalMs) return;
    lastState = now;
    opts.onState({
      phase,
      tracking,
      aiming: phase === 'placing' && placer.aiming,
      realScale,
      zoom,
      canOverlay,
      locate: phase === 'locating' && locator ? locator.progress() : null,
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
      const placement = placeModel(opts.info, sampleVertices(gltf.scene), opts.scene);
      if (placement.keepWithin !== null) cropMesh(gltf.scene, new THREE.Vector3(...opts.info.center), placement.keepWithin);
      tablePlacement = placement.matrix;
      inner.matrix.copy(tablePlacement);
      inner.add(gltf.scene);
      model = gltf;
      realScale = placement.realScale;
      phase = locator ? 'locating' : 'placing';
      emit(true);
    })
    .catch(() => {
      error = 'メッシュを読み込めませんでした。';
      emit(true);
    });

  const pos = new THREE.Vector3();
  const quat = new THREE.Quaternion();
  const fwd = new THREE.Vector3();

  /** 机に置く形にする（照準から）。 */
  function toPlacing() {
    anchored.visible = false;
    keeper.clear();
    inner.matrix.copy(tablePlacement);
    pivot.position.set(0, 0, 0);
    pivot.rotation.set(0, 0, 0);
    pivot.scale.setScalar(zoom);
    phase = 'placing';
    emit(true);
  }

  /** 実物に重ねる: 地図の座標をそのまま AR の座標にし、重ねた所（被写体の中心）のアンカーに貼り付ける。 */
  function toOverlay(matrix: THREE.Matrix4) {
    pivot.position.set(0, 0, 0);
    pivot.rotation.set(0, 0, 0);
    pivot.scale.setScalar(1);
    inner.matrix.copy(matrix);
    keeper.anchor(new THREE.Vector3(...opts.info.center).applyMatrix4(matrix));
    anchored.matrix.identity();
    anchored.visible = true;
    phase = 'overlay';
    emit(true);
  }

  // 画面のタップ（DOM の部品の上以外）で置く。
  session.addEventListener('select', () => {
    if (phase !== 'placing' || !placer.aiming || !tracking) return;
    const p = placer.position();
    pivot.position.copy(p);
    // モデルの正面（+z）を、利用者のいる方へ向ける。
    pivot.rotation.set(0, Math.atan2(pos.x - p.x, pos.z - p.z), 0);
    keeper.anchor(p);
    anchored.matrix.identity();
    anchored.visible = true;
    placer.hide();
    phase = 'placed';
    emit(true);
  });

  session.addEventListener('end', () => {
    ended = true;
    renderer.setAnimationLoop(null);
    locator?.dispose();
    keeper.clear();
    placer.dispose();
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

  renderer.setAnimationLoop((now: number, frame?: XRFrame) => {
    if (!frame) return;
    const pose = frame.getViewerPose(refSpace);
    // emulatedPosition: AR が位置を見失い、向きしか分かっていない。置かせず、メーターも進めない。
    tracking = !!pose && !pose.emulatedPosition;
    if (pose) {
      const t = pose.views[0]!.transform;
      pos.set(t.position.x, t.position.y, t.position.z);
      quat.set(t.orientation.x, t.orientation.y, t.orientation.z, t.orientation.w);
      fwd.set(0, 0, -1).applyQuaternion(quat);
    }
    anchored.matrix.copy(keeper.update(frame, refSpace));

    if (phase === 'placing') placer.update(frame, refSpace, tracking ? { pos, quat, fwd } : null, now);
    else placer.hide();

    if (phase === 'locating' && locator && pose) {
      const image = cameraImageOf(renderer, pose.views[0]!);
      // カメラ画像が来ない端末（camera-access に対応していない）なら、重ねるのをあきらめて机に置く。
      if (!image) {
        noImageSince ??= now;
        if (now - noImageSince > noImageFallbackMs) {
          canOverlay = false;
          toPlacing();
        }
      } else {
        noImageSince = null;
      }
      locator.update(now, tracking ? { pos, fwd } : null, image);
      const m = locator.alignment();
      if (m && phase === 'locating') toOverlay(m);
    }

    renderer.render(scene, camera);
    emit();
  });

  emit(true);

  const setZoom = (z: number) => {
    zoom = Math.min(maxZoom, Math.max(minZoom, z));
    if (phase === 'placed' || phase === 'placing') pivot.scale.setScalar(zoom);
    emit(true);
  };

  return {
    place() {
      if (phase === 'loading') return;
      toPlacing();
    },
    overlay() {
      if (!locator || phase === 'loading') return;
      anchored.visible = false;
      keeper.clear();
      locator.reset();
      phase = 'locating';
      emit(true);
    },
    rotateBy(radians) {
      if (phase === 'placed') pivot.rotation.y += radians;
    },
    zoomBy(factor) {
      setZoom(zoom * factor);
    },
    resetZoom() {
      setZoom(1);
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
