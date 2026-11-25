// AR で見る: 出来上がったメッシュを、スマホの AR（WebXR / ARCore）で現実の机や床の上に置いて見る。
//
// 流れ:
//   placing  照準（輪）が出る。タップした所にメッシュを置く（arPlacement.ts。白い机でも空中に置ける）
//   placed   1本指で左右になぞると回る、2本指でつまむと大きさが変わる（画面側 ArViewHud.tsx が呼ぶ）
//
// 向き・大きさは arModel.ts: 真上を合わせ、底を置いた面に乗せ、最初に撮った側を利用者に向ける。
// 物を囲んで撮ったスキャンは、撮ったカメラの輪の内側だけを残す（背景の膜を取り除く）。
// AR で撮ったスキャンは実物大（scene.json の metersPerUnit）。それ以外は「カメラが 60cm 先にいた」とした目安の大きさ。
//
// 対応: Android の Chrome（ARCore 対応機種）。カメラ画像は使わないので camera-access は頼まない。
import * as THREE from 'three';
import { GLTFLoader, type GLTF } from 'three/addons/loaders/GLTFLoader.js';
import type { SceneInfo } from '../api/types';
import { createPlacer } from '../capture/arPlacement';
import { cropMesh, placeModel, sampleVertices } from './arModel';
import { unlit } from './materials';

export type ArViewPhase = 'loading' | 'placing' | 'placed';

export interface ArViewState {
  phase: ArViewPhase;
  /** 照準が出ている（タップで置ける）。 */
  aiming: boolean;
  /** 実物大で見られる（AR で撮ったスキャン）。 */
  realScale: boolean;
  /** 実物大（または目安の大きさ）に対する倍率。 */
  zoom: number;
  /** メッシュを読み込めなかった。 */
  error: string | null;
}

export interface ArViewController {
  /** 置き直す。 */
  replace(): void;
  /** 鉛直の軸のまわりに回す（ラジアン）。 */
  rotateBy(radians: number): void;
  /** 大きさを掛ける。 */
  zoomBy(factor: number): void;
  /** 元の大きさ（実物大）に戻す。 */
  resetZoom(): void;
  /** AR を閉じる。 */
  end(): void;
}

/** 大きさの倍率の範囲。 */
const minZoom = 0.1;
const maxZoom = 10;
const stateIntervalMs = 100;

/**
 * AR で見始める。利用者の操作（タップ）の中で、ほかの await より先に呼ぶこと。
 * @param overlay AR の上に重ねる画面の根の要素
 */
export async function startArView(opts: {
  overlay: HTMLElement;
  meshUrl: string;
  info: SceneInfo;
  /** 風景モードで撮ったスキャン（切り抜かず、全体を模型の大きさにする）。 */
  scene?: boolean;
  onState: (s: ArViewState) => void;
  onEnded: () => void;
}): Promise<ArViewController> {
  const xr = navigator.xr;
  if (!xr) throw new Error('この端末では AR を使えません。');
  const sessionPromise = xr.requestSession('immersive-ar', {
    requiredFeatures: ['local', 'hit-test', 'dom-overlay'],
    domOverlay: { root: opts.overlay },
  } as XRSessionInit);
  // メッシュは AR を始めるのと並べて読み込む。
  const gltfPromise = new GLTFLoader().loadAsync(opts.meshUrl);
  gltfPromise.catch(() => {}); // 失敗は下で扱う（未処理の拒否にしない）

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

  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera();
  scene.add(placer.group);
  // pivot: 置いた点（位置）・利用者の回した向き・倍率。inner: モデルの座標 → 置いた点を原点とする座標。
  const pivot = new THREE.Group();
  pivot.visible = false;
  const inner = new THREE.Group();
  inner.matrixAutoUpdate = false;
  pivot.add(inner);
  scene.add(pivot);

  let phase: ArViewPhase = 'loading';
  let realScale = false;
  let zoom = 1;
  let error: string | null = null;
  let ended = false;
  let lastState = 0;
  let model: GLTF | null = null;

  const emit = (force = false) => {
    const now = performance.now();
    if (!force && now - lastState < stateIntervalMs) return;
    lastState = now;
    opts.onState({ phase, aiming: phase === 'placing' && placer.aiming, realScale, zoom, error });
  };

  gltfPromise
    .then((gltf) => {
      if (ended) return;
      gltf.scene.traverse((obj) => {
        const mesh = obj as THREE.Mesh;
        if (mesh.isMesh) mesh.material = unlit(mesh.material);
      });
      const placement = placeModel(opts.info, sampleVertices(gltf.scene), opts.scene);
      if (placement.keepWithin !== null) cropMesh(gltf.scene, new THREE.Vector3(...opts.info.center), placement.keepWithin);
      inner.matrix.copy(placement.matrix);
      inner.add(gltf.scene);
      model = gltf;
      realScale = placement.realScale;
      phase = 'placing';
      emit(true);
    })
    .catch(() => {
      error = 'メッシュを読み込めませんでした。';
      emit(true);
    });

  const pos = new THREE.Vector3();
  const quat = new THREE.Quaternion();
  const fwd = new THREE.Vector3();

  // 画面のタップ（DOM の部品の上以外）で置く。
  session.addEventListener('select', () => {
    if (phase !== 'placing' || !placer.aiming) return;
    const p = placer.position();
    pivot.position.copy(p);
    // モデルの正面（+z）を、利用者のいる方へ向ける。
    pivot.rotation.set(0, Math.atan2(pos.x - p.x, pos.z - p.z), 0);
    pivot.visible = true;
    placer.hide();
    phase = 'placed';
    emit(true);
  });

  session.addEventListener('end', () => {
    ended = true;
    renderer.setAnimationLoop(null);
    placer.dispose();
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
    if (pose) {
      const t = pose.views[0]!.transform;
      pos.set(t.position.x, t.position.y, t.position.z);
      quat.set(t.orientation.x, t.orientation.y, t.orientation.z, t.orientation.w);
      fwd.set(0, 0, -1).applyQuaternion(quat);
    }
    if (phase === 'placing') placer.update(frame, refSpace, pose ? { pos, quat, fwd } : null, now);
    else placer.hide();
    renderer.render(scene, camera);
    emit();
  });

  emit(true);

  const setZoom = (z: number) => {
    zoom = Math.min(maxZoom, Math.max(minZoom, z));
    pivot.scale.setScalar(zoom);
    emit(true);
  };

  return {
    replace() {
      if (phase !== 'placed') return;
      pivot.visible = false;
      phase = 'placing';
      emit(true);
    },
    rotateBy(radians) {
      pivot.rotation.y += radians;
    },
    zoomBy(factor) {
      setZoom(zoom * factor);
    },
    resetZoom() {
      setZoom(1);
    },
    end() {
      if (!ended) void session.end().catch(() => {});
    },
  };
}
