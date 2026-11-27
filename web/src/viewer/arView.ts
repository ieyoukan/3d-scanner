// AR で見る: 出来上がったメッシュを、撮った場所で実物に重ねて見比べる（スマホの AR、WebXR / ARCore）。
//
// 流れ:
//   ready     カメラの映像が見えている。「重ねる」を押すと、その瞬間のカメラ画像を位置合わせ（VPS）に送る
//   checking  照合中。経過秒数と目安（前回かかった秒数）をメーターで見せる。使うのは押した瞬間の写真と向きなので、
//             その場に居れば、待つ間に向きを変えてよい
//   overlay   重なった。メッシュを、写真を撮った瞬間のカメラから見た形でぴったり重ねる。透かして実物と見比べられる。
//             重なっている間も、裏で照合を繰り返して合わせ直す（下の「合わせ直し」）
//   failed    見つからなかった・断られた。理由とやり直し方を見せる
//
// なぜ AR の「位置」を使わないのか: 実機の記録では、AR（ARCore）の位置（移動）は 40 秒の撮影で 18m 以上飛ぶほど
// 狂っていた（その場で 1m 以上ずれる）。一方、向き（回転）は写真から求めた向きと 1.5° 以内で合っていた。
// そこで、重ねる瞬間の写真 1 枚の位置合わせで「地図でのカメラの位置と向き」を求め、その瞬間の AR の向きとそろえる
// （resume/arAlign.ts の snapshotPose）。以後は AR の向きだけで追い、メッシュをカメラの位置に対して固定する。
// その場で見回す分には合ったまま。歩いて場所を変えたら「重ね直す」。
//
// 合わせ直し: AR の向き（ジャイロ）も少しずつずれる（実機で、重ねた直後は合っていたのが、じわじわずれた）。
// そこで重なった後は、前の照合が返るたびに次の写真を送り、見つかったら新しい重ね方へ blendMs かけて寄せる。
// 前回分かった位置といまの向きを手がかりに付け、近くの地図の写真とだけ照合させる（本番で 1 回 約 12 秒。
// 手がかり無しは 約 40 秒）。手がかり付きで続けて見つからなければ、ときどき手がかり無しで照合する。
// 回転が maxAutoTurnDeg より大きく変わる結果は取り違えとみなして使わない。端末を速く回している間は、
// 写真がぶれるので送らない。
//
// 物を囲んで撮ったスキャンは、撮ったカメラの輪の内側だけを残す（背景の膜が画面を覆わないように）。
import * as THREE from 'three';
import { GLTFLoader, type GLTF } from 'three/addons/loaders/GLTFLoader.js';
import type { LocalizeHint, LocalizeResult, SceneInfo } from '../api/types';
import { cameraImageOf, makeGrabber } from '../capture/arCamera';
import { frameFromVideoFrame, releaseFrame } from '../capture/frames';
import {
  blendPose,
  mapForward,
  poseMatrix,
  poseTurnDegrees,
  snapshotPose,
  type Quat,
  type SnapshotPose,
} from '../resume/arAlign';
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
  /** 重なった後、裏で合わせ直しの照合をしている。 */
  realigning: boolean;
  /** 最後に合わせた（重ねた・合わせ直した）ときからの秒数。重なっていなければ null。 */
  sinceFixSeconds: number | null;
  /** 合わせ直しが続けてうまくいかなかった回数（0 なら直近は合わせ直せている）。 */
  realignMisses: number;
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
/** 前回の照合にかかった秒数を覚えておく場所（目安に使う）。手がかり無しの照合（押したとき）だけ数える。 */
const expectedKey = 'ar.localizeSeconds';
/** 合わせ直しで、新しい重ね方へ寄せるのにかける時間（ミリ秒）。 */
const blendMs = 600;
/** 合わせ直しの照合が返ってから、次を送るまでの間（ミリ秒）。 */
const autoGapMs = 300;
/** 合わせ直しが通信やサーバーの問題で失敗したとき、次を送るまでの間（ミリ秒）。 */
const autoErrorGapMs = 5000;
/** 合わせ直しで、これより大きく回る結果は取り違えとみなす（度）。ジャイロのずれは 1 回の照合の間にここまで溜まらない。 */
const maxAutoTurnDeg = 20;
/** これより速く端末を回している間は、合わせ直しの写真を送らない（度/秒）。 */
const steadyDegPerSec = 40;
/** 手がかり付きで続けて見つからなかったとき、何回に 1 回を手がかり無しにするか。 */
const fullEvery = 3;
/** 押したときの照合が「処理中」で断られたら、この間をおいて送り直す（裏の合わせ直しを止めた直後に起こる）。 */
const busyRetryMs = 2500;
const busyGiveUpMs = 45000;

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
  /** near（前回分かった位置といまの向き）を付けると、その近くの地図の写真とだけ照合する。 */
  localize: (image: Blob, signal: AbortSignal, near?: LocalizeHint) => Promise<LocalizeResult>;
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
  //   follow  毎フレーム、AR のカメラの位置に置く（メッシュをカメラの位置に対して固定する）
  //   inner   地図の座標 → カメラの位置を原点とした AR の座標（poseMatrix。地図でのカメラの位置が原点に来る）
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
  /** 次のフレームで「押したとき」の照合を送る。 */
  let alignRequested = false;
  /** 押したときの照合が「処理中」で断られた。この時刻に送り直す。 */
  let manualRetryAt = Infinity;
  let checkStart = 0;
  let now = 0;
  /** 送っている照合（押したとき・裏の合わせ直し）と、それを止めるもの。 */
  let inFlight: 'manual' | 'auto' | null = null;
  let abort: AbortController | null = null;
  /** いま描いている重ね方と、合わせ直しで寄せている途中なら、その始めと終わり。 */
  let pose: SnapshotPose | null = null;
  let blendFrom: SnapshotPose | null = null;
  let blendTo: SnapshotPose | null = null;
  let blendStart = 0;
  /** 次の合わせ直しを送ってよい時刻。 */
  let autoAt = Infinity;
  /** 合わせ直しが続けて見つからなかった回数（手がかりを外す目安）と、何であれうまくいかなかった回数（画面用）。 */
  let autoNotFound = 0;
  let autoFailures = 0;
  let lastFixAt = 0;
  /** 前のフレームの AR のカメラの向き（端末を回す速さを測る）。 */
  const prevQuat = new THREE.Quaternion();
  const quat = new THREE.Quaternion();
  let prevT = -1;
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
      realigning: inFlight === 'auto',
      sinceFixSeconds: phase === 'overlay' && lastFixAt > 0 ? Math.max(0, (now - lastFixAt) / 1000) : null,
      realignMisses: autoFailures,
    });
  };

  /** すぐにこの重ね方にする（押したときの照合）。 */
  function setPose(next: SnapshotPose) {
    pose = next;
    blendFrom = blendTo = null;
    poseMatrix(next, scale, undefined, inner.matrix);
    inner.matrixWorldNeedsUpdate = true;
  }

  /** この重ね方へ少しずつ寄せる（合わせ直し）。 */
  function blendToPose(next: SnapshotPose) {
    if (!pose) return setPose(next);
    blendFrom = pose;
    blendTo = next;
    blendStart = now;
  }

  function updateBlend(t: number) {
    if (!blendFrom || !blendTo) return;
    const k = Math.min(1, Math.max(0, (t - blendStart) / blendMs));
    if (k >= 1) {
      setPose(blendTo);
      return;
    }
    pose = blendPose(blendFrom, blendTo, k * k * (3 - 2 * k));
    poseMatrix(pose, scale, undefined, inner.matrix);
    inner.matrixWorldNeedsUpdate = true;
  }

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

  /**
   * いまのフレームのカメラ画像と AR の向きで、位置合わせに送る。
   * @param kind   manual: 押したとき（手がかり無し。結果ですぐ重ねる）。auto: 重なった後の合わせ直し（手がかり付き。少しずつ寄せる）
   * @param retry  押したときの照合を「処理中」で断られて送り直す（メーターの経過秒数を続ける）
   */
  function sendSnapshot(view: XRView, kind: 'manual' | 'auto', retry = false) {
    const image = cameraImageOf(renderer, view);
    if (!image) {
      if (kind === 'manual') {
        failure = 'カメラの画像を受け取れませんでした。もう一度押してください。';
        phase = 'failed';
        emit(true);
      } else {
        autoAt = now + autoErrorGapMs;
      }
      return;
    }
    const o = view.transform.orientation;
    const arQuat: Quat = [o.x, o.y, o.z, o.w];
    // 合わせ直しは、前回分かった位置といまの向きを手がかりにする。続けて見つからないときは、ときどき地図全体と照合する。
    const near: LocalizeHint | undefined =
      kind === 'auto' && pose && autoNotFound % fullEvery !== fullEvery - 1
        ? { position: [pose.camera.x, pose.camera.y, pose.camera.z], forward: mapForward(pose, arQuat) }
        : undefined;
    if (kind === 'manual') {
      if (!retry) checkStart = now;
      phase = 'checking';
      failure = null;
      emit(true);
    }
    inFlight = kind;
    const controller = new AbortController();
    abort = controller;
    void (async () => {
      let bitmap: ImageBitmap | undefined;
      try {
        bitmap = await grab.snapshot(image.tex, image.width, image.height, queryEdge);
        const frame = await frameFromVideoFrame(bitmap, new Date().toISOString());
        releaseFrame(frame);
        const result = await opts.localize(frame.blob, controller.signal, near);
        if (ended || controller.signal.aborted) return;
        if (kind === 'manual' && result.durationMs > 0) {
          expectedSeconds = Math.round(result.durationMs / 1000);
          writeExpected(expectedSeconds);
        }
        const { position, forward, down } = result;
        const next = result.localized && position && forward && down ? snapshotPose({ position, forward, down }, arQuat) : null;
        if (kind === 'auto') {
          // 見つからない・回り過ぎ（取り違え）なら今の重ね方のまま、次を送る。
          if (!next || (pose && poseTurnDegrees(pose, next) > maxAutoTurnDeg)) {
            autoNotFound += 1;
            autoFailures += 1;
            autoAt = now + autoGapMs;
            emit(true);
            return;
          }
          blendToPose(next);
        } else {
          if (!next) {
            matches = null;
            failure = '見つかりませんでした。撮ったときと同じ所から、撮ったものが大きく写るように向けて、もう一度押してください。';
            phase = 'failed';
            emit(true);
            return;
          }
          setPose(next);
          follow.visible = true;
          phase = 'overlay';
        }
        matches = result.matches;
        lastFixAt = now;
        autoNotFound = 0;
        autoFailures = 0;
        autoAt = now + autoGapMs;
        emit(true);
      } catch (err) {
        if (ended || controller.signal.aborted) return;
        const busy = err instanceof Error && /処理中|busy/.test(err.message);
        if (kind === 'auto') {
          autoFailures += 1;
          autoAt = now + autoErrorGapMs;
          emit(true);
          return;
        }
        // 裏の合わせ直しを止めた直後は、サーバーがまだそれを処理していて断られることがある。少し待って送り直す。
        if (busy && now - checkStart < busyGiveUpMs) {
          manualRetryAt = now + busyRetryMs;
          return;
        }
        failure = busy
          ? '別の位置合わせを処理中です。少し待ってからもう一度押してください。'
          : '照合できませんでした（通信やサーバーの問題）。もう一度押してください。';
        phase = 'failed';
        emit(true);
      } finally {
        bitmap?.close();
        if (abort === controller) {
          abort = null;
          inFlight = null;
        }
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
    const viewer = frame.getViewerPose(refSpace);
    if (viewer) {
      const view = viewer.views[0]!;
      const o = view.transform.orientation;
      quat.set(o.x, o.y, o.z, o.w);
      const turnRate = prevT >= 0 && t > prevT ? THREE.MathUtils.radToDeg(quat.angleTo(prevQuat)) / ((t - prevT) / 1000) : 0;
      prevQuat.copy(quat);
      prevT = t;
      if (alignRequested || t >= manualRetryAt) {
        const retry = !alignRequested;
        alignRequested = false;
        manualRetryAt = Infinity;
        sendSnapshot(view, 'manual', retry);
      } else if (phase === 'overlay' && inFlight === null && t >= autoAt && turnRate < steadyDegPerSec) {
        sendSnapshot(view, 'auto');
      }
      updateBlend(t);
      // メッシュをカメラの位置に置く（AR の位置は狂いやすいので、位置の変化はそのまま打ち消す）。
      const p = view.transform.position;
      follow.position.set(p.x, p.y, p.z);
    }
    renderer.render(scene, camera);
    emit();
  });

  emit(true);

  return {
    align() {
      if (phase === 'loading' || phase === 'checking' || error) return;
      // 裏の合わせ直しは止める（押した瞬間の写真で、地図全体から探し直す）。
      if (inFlight === 'auto') {
        abort?.abort();
        abort = null;
        inFlight = null;
      }
      autoAt = Infinity;
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
