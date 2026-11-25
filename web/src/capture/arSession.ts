// AR で撮る: スマホの AR（WebXR / ARCore）でカメラの位置を追いながら撮り、どの方向から撮れたかを
// 対象のまわりのドームに色で重ねて見せる。
//
// 流れ:
//   placing    照準（輪）が出る。対象に合わせてタップすると、そこを中心にドームを置く。
//              輪は、床や机の面・対象の表面の特徴点に当たればそこに付く。白い机など何も見つからないときは
//              少し待ってから、前方の空中（前に当たった距離、無ければ 0.6 m 先）に浮かべて出す
//   ready      録画ボタンで撮り始める
//   recording  1/3 秒ごとに一番くっきりしたカメラ画像を写真にする（動画で撮ると同じ選び方）。
//              対象を向いて撮った方向のタイルが緑になる。次に撮るタイルは琥珀色で、画面の端に矢印が出る
//   finishing  停止したら、最後の写真ができるのを待って AR を終える
//
// 風景モード（scene: true）は、物を囲まずに歩きながら空間を撮る。中心もドームも無く、すぐ ready から始める:
//   写真    時間ではなく「動いた量」（15cm か 12°）ごとの区間で一番くっきりしたコマ（sceneMotion.ts）。
//           立ち止まっても同じ写真が溜まらない。サーバーに送る写真は長辺 1600px（枚数が多いので）
//   画面    歩いた道を真上から見た小さな図（撮った所の点と向き）。その場で向きだけ変えている・速すぎるときは警告
//
// 続きを撮る（reference を渡したとき）は、中心を置く代わりに locating から始める:
//   locating   カメラ画像を位置合わせ（VPS）に送り、前回の3Dデータ（地図）の座標を AR に重ねる（arLocator.ts）。
//              画面のメーターで「周りを覚える → 照合 → （横へ）」の進み具合を見せる。
//              重なったら、ドームの中心を前回の被写体の中心に置き、前回撮った方向のタイルを最初から緑にし、
//              前回のメッシュを半透明で重ねる。大きさが分からないスキャンは、横に動いてもう1か所で合わせる
//
// アンカー（arAnchor.ts）: ドームと前回のメッシュは、置いた（重ねた）所のアンカーに結び付ける。AR が自分の位置の推定を
// 直しても、現実に貼り付いたままになる。位置を見失っている間（AR が向きしか分からない）は、置く・撮れた方向を数える・
// 位置合わせを止める。
//
// 中心の寄せ直し: 撮影中、カメラの視線が集まる点（対象の中心）を求め、置いた中心をそこへ少しずつ寄せる。
// 空中に置いた・対象の手前の面に置いた等で中心がずれていても、回るうちに対象の中心に合っていく。
// 中心を動かしたら、撮れた方向はそれまでのカメラの位置と視線から数え直す。
//
// カメラ画像は WebXR の camera-access で受け取る（three.js が毎フレーム ExternalTexture にしてくれる）。
// 画像は GPU で描き直してから読み出す:
//   くっきり度  長辺 480px に縮めて同期で読む（小さいので速い）
//   写真        区間で一番くっきりしたコマを、フル解像度の描画先に描いて控える。区間が確定したら非同期で読み出す
// 読み出した画素は上下が逆になる（GL の約束）ので、描くときに上下を反転して、上の行から並ぶようにしておく。
//
// 対応: Android の Chrome（ARCore 対応機種）。iPhone の Safari は WebXR の AR に対応していない。
import * as THREE from 'three';
import type { LocalizeResult, SceneInfo } from '../api/types';
import { cameraDistance, cropMesh, keepFraction } from '../viewer/arModel';
import { createAnchorKeeper } from './arAnchor';
import { cameraImageOf, makeGrabber, photoEdge } from './arCamera';
import { createLocator, type LocateProgress } from './arLocator';
import { createPlacer } from './arPlacement';
import {
  convergePoint,
  coverageOf,
  currentTile,
  directionOf,
  nextTarget,
  tileAt,
  tiles,
  type PoseSample,
  type Vec3,
} from './coverage';
import { releaseFrame, sceneEncoding, type ArPose, type Frame } from './frames';
import { MotionMeter, MotionWarnings, type MotionWarning } from './sceneMotion';
import { thinPairs } from './liveVideo';
import { BestPerWindow, FrameCollector, framesPerSecond } from './video';

/** くっきり度を測る最短の間隔（ミリ秒）。毎フレーム読み出すと AR の描画が遅れるので、間引く。 */
const scoreIntervalMs = 45;
/** 状態を画面に知らせる最短の間隔（ミリ秒）。 */
const stateIntervalMs = 100;
/** 中心の寄せ直しに使うカメラの位置と視線を控える間隔（ミリ秒）と、寄せ直す間隔。 */
const sampleIntervalMs = 100;
const refineIntervalMs = 500;
/** 1回の寄せ直しで、求めた点へ寄せる割合（急に飛ばないように）。 */
const refineRate = 0.35;

export type ArPhase = 'locating' | 'placing' | 'ready' | 'recording' | 'finishing';

/** 続きを撮るときに重ねる、前回の3Dデータ。 */
export interface ArReference {
  info: SceneInfo;
  /** 前回のメッシュ（GLB）。無ければ重ねない。 */
  meshUrl: string | null;
  /** 写真1枚を前回の地図に位置合わせする（サーバーの VPS）。 */
  localize: (image: Blob, signal: AbortSignal) => Promise<LocalizeResult>;
}

/** 位置合わせの進み具合（locating の間）。 */
export type LocateState = LocateProgress;

export interface ArState {
  phase: ArPhase;
  /** 端末の位置が分かっている（まだなら、端末をゆっくり動かしてもらう）。 */
  tracking: boolean;
  /** 照準が出ている（タップで中心を置ける）。 */
  aiming: boolean;
  /** タイルごとの「撮れた」。 */
  covered: boolean[];
  /** 次に撮るところ。全部撮れたら null。 */
  target: ReturnType<typeof nextTarget>;
  /** いま対象を向いているか（向いていないと撮れたことにならない）。 */
  facing: boolean;
  /** 中心から見た、いまいる方向（方位角、度）。中心を置く前は null。 */
  az: number | null;
  picked: number;
  elapsed: number;
  /** この端末ではカメラ画像を受け取れない（撮れない）。 */
  noCameraImage: boolean;
  /** 位置合わせの進み具合（続きを撮るときの locating の間だけ）。 */
  locate: LocateState | null;
  /** 前回のメッシュを重ねて見せているか（重ねるメッシュが無ければ null）。 */
  meshShown: boolean | null;
  /** 風景モード（中心もドームも無い）。 */
  scene: boolean;
  /** 風景モード: 撮った写真の位置と向き（真上から見た x, z と、方位角 yaw ラジアン）。 */
  path: PathPoint[];
  /** 風景モード: いまの位置と向き。 */
  here: PathPoint | null;
  /** 風景モード: その場で向きだけ変えている（spin）・速すぎる（fast）。 */
  warn: MotionWarning | null;
}

/** 真上から見た位置と向き（yaw は +x 方向が正の方位角。atan2(視線の x, 視線の -z)）。 */
export interface PathPoint {
  x: number;
  z: number;
  yaw: number;
}

export interface ArController {
  /** 撮り始める。 */
  record(): void;
  /** 撮り終えて AR を閉じ、写真を撮った順に返す。 */
  stop(): Promise<Frame[]>;
  /** AR を閉じ、写真を捨てる。 */
  cancel(): void;
  /** 中心を置き直す（撮る前だけ）。 */
  replace(): void;
  /** 前回のメッシュを見せる・隠す。 */
  toggleMesh(): void;
}

/**
 * AR を始める。利用者の操作（タップ）の中で、ほかの await より先に呼ぶこと
 * （ブラウザは、操作の直後でないと AR の開始を許さない）。
 *
 * @param overlay AR の上に重ねる画面（DOM overlay）の根の要素
 * @param room    追加できる残りの枚数（超えないよう間引く）
 */
export async function startAr(opts: {
  overlay: HTMLElement;
  room: number;
  onState: (s: ArState) => void;
  onEnded: () => void;
  /** 続きを撮る: 前回の3Dデータに重ねる。 */
  reference?: ArReference;
  /** 風景モード: 歩きながら空間を撮る。 */
  scene?: boolean;
}): Promise<ArController> {
  const xr = navigator.xr;
  if (!xr) throw new Error('この端末では AR を使えません。');
  // 最初に頼む（この前に await を挟まない）。
  const sessionPromise = xr.requestSession('immersive-ar', {
    // dom-overlay が無いと録画ボタンなどを出せず操作できなくなるので、必須にする。
    requiredFeatures: ['local', 'hit-test', 'camera-access', 'dom-overlay'],
    // anchors: 置いたドームを現実に貼り付ける。対応していない端末でも AR は始める（貼り付けなしで動く）。
    optionalFeatures: ['anchors'],
    domOverlay: { root: opts.overlay },
  } as XRSessionInit);

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
        ? 'この端末では AR で撮れません（カメラ画像を受け取れません）。'
        : 'AR を始められませんでした。',
    );
  }
  await renderer.xr.setSession(session);
  const refSpace = renderer.xr.getReferenceSpace()!;
  // 照準（面・特徴点・空中。arPlacement.ts）。
  const placer = await createPlacer(session);

  // ---- 3D の表示 ----
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera();
  scene.add(placer.group);
  // 置いた物（ドーム・前回のメッシュ）はこのグループに入れ、アンカーの補正（AR の位置の推定の直し）を掛ける。
  const anchored = new THREE.Group();
  anchored.matrixAutoUpdate = false;
  scene.add(anchored);
  const keeper = createAnchorKeeper();
  let correction = new THREE.Matrix4();
  let dome: Dome | null = null;
  /** ドームの中心（置いたときの AR の座標）。いまの AR の座標では correction を掛けたもの（centerNow）。 */
  let center: Vec3 | null = null;
  const centerNow = (): Vec3 | null => {
    if (!center) return null;
    const v = new THREE.Vector3(...center).applyMatrix4(correction);
    return [v.x, v.y, v.z];
  };

  // ---- 続きを撮る: 前回の3Dデータを重ねる ----
  const reference = opts.reference ?? null;
  /** 地図の座標 → AR の座標（位置合わせで決まる）。前回のメッシュはこの下に置く。 */
  const mapGroup = new THREE.Group();
  mapGroup.matrixAutoUpdate = false;
  mapGroup.visible = false;
  anchored.add(mapGroup);
  let meshShown = true;
  let hasMesh = false;
  let disposeMesh = () => {};
  if (reference?.meshUrl) {
    void loadOverlayMesh(reference.meshUrl, reference.info).then((loaded) => {
      if (!loaded) return;
      if (ended) {
        loaded.dispose();
        return;
      }
      mapGroup.add(loaded.object);
      disposeMesh = loaded.dispose;
      hasMesh = true;
      emit(performance.now(), true);
    });
  }

  // ---- 風景モード ----
  const sceneMode = !!opts.scene && !reference;
  const meter = new MotionMeter();
  const warnings = new MotionWarnings();
  let warn: MotionWarning | null = null;

  // ---- 状態 ----
  let phase: ArPhase = reference ? 'locating' : sceneMode ? 'ready' : 'placing';
  let tracking = false;
  /** 撮影中のカメラの位置と視線（中心の寄せ直しと、撮れた方向の数え直し用）。 */
  const samples: PoseSample[] = [];
  let lastSample = 0;
  let lastRefine = 0;
  /** 中心を、空中（面や特徴点に当たらない所）に置いた。高さも視線から寄せ直す。 */
  let placedInAir = false;
  let domeBaseRadius = 0;
  const covered = tiles.map(() => false);
  let facing = false;
  let az: number | null = null;
  let target: ReturnType<typeof nextTarget> = null;
  let noCameraImage = false;
  let ended = false;
  let lastState = 0;
  let recordStart = 0;
  let elapsed = 0;

  // ---- 写真の選び方（liveVideo.ts と同じ: 1/3 秒ごとの区間で一番くっきりしたもの、上限で間引く） ----
  // 風景モードは、時間の代わりに動いた量（MotionMeter の座標。1 = 15cm か 12°）で区間を切る。
  const room = Math.max(1, opts.room);
  let stage = 0;
  let stageStart = 0;
  /** 区間を数える座標のいまの値（物: 撮り始めてからの秒数、風景: 動いた量）。 */
  let progress = 0;
  let interval = sceneMode ? 1 : 1 / framesPerSecond;
  let picker = BestPerWindow.every(interval);
  const key = (w: number) => stage * 1_000_000 + w;
  const collector = new FrameCollector(null, () => {
    if (collector.frames.length >= room && room >= 2) {
      const { kept, dropped } = thinPairs(collector.sorted());
      dropped.forEach(releaseFrame);
      collector.replace(kept);
      closeCandidate(picker.finish().map(key));
      stage += 1;
      stageStart = progress;
      interval *= 2;
      picker = BestPerWindow.every(interval);
    }
  }, sceneMode ? sceneEncoding : undefined);
  const grab = makeGrabber(renderer);
  // 続きを撮る: 前回の地図に重ねるための位置合わせ（メーターの段階も持つ）。
  const locator = reference ? createLocator({ info: reference.info, localize: reference.localize, grab }) : null;
  /** いま控えている候補の区間と撮影時刻と、そのときのカメラの位置と向き。 */
  let candidate: { key: number; capturedAt: string; pose: ArPose } | null = null;
  /** この AR の回の印（写真のカメラ位置に付けて送る。AR の座標系は回ごとに違うため）。 */
  const arSessionId = crypto.randomUUID();
  let lastScore = 0;

  /** 確定した区間に控えていた候補を、写真にする。 */
  function closeCandidate(keys: number[]) {
    if (!candidate || !keys.includes(candidate.key)) return;
    collector.add(grab.readCandidate(), candidate.capturedAt, candidate.pose);
    candidate = null;
  }

  function emit(now: number, force = false) {
    if (!force && now - lastState < stateIntervalMs) return;
    lastState = now;
    opts.onState({
      phase,
      tracking,
      aiming: phase === 'placing' && placer.aiming,
      covered: [...covered],
      target,
      facing,
      az,
      picked: collector.frames.length,
      elapsed,
      noCameraImage,
      locate: phase === 'locating' && locator ? locator.progress() : null,
      meshShown: hasMesh ? meshShown : null,
      scene: sceneMode,
      path: sceneMode ? pathOf(collector.frames) : [],
      here: sceneMode && tracking ? { x: pos.x, z: pos.z, yaw: Math.atan2(fwd.x, -fwd.z) } : null,
      warn,
    });
  }

  // 毎フレームのカメラの位置と向き（中心を置くときにも使う）。
  const pos = new THREE.Vector3();
  const quat = new THREE.Quaternion();
  const fwd = new THREE.Vector3();

  // 画面のタップ（DOM の部品の上以外）。置く段階なら、照準の位置に中心を置く。
  session.addEventListener('select', () => {
    if (phase !== 'placing' || !placer.aiming) return;
    const p = placer.position();
    center = [p.x, p.y, p.z];
    placedInAir = placer.inAir;
    // ドームの大きさは、置いたときの対象までの距離の 4 割（小さすぎ・大きすぎは丸める）。
    domeBaseRadius = domeRadius(Math.hypot(pos.x - p.x, pos.z - p.z));
    dome = makeDome(domeBaseRadius);
    // いまの AR の座標で置き、アンカーで現実に貼り付ける（以後の補正は置いた時点からの差なので、ここで単位行列に戻る）。
    keeper.anchor(p);
    correction = new THREE.Matrix4();
    dome.group.position.copy(p);
    anchored.add(dome.group);
    placer.hide();
    phase = 'ready';
    emit(performance.now(), true);
  });

  /**
   * 撮影中のカメラの視線が集まる点へ、中心を少し寄せる。中心が動いたら、撮れた方向を数え直す。
   * 面に置いたときは高さは置いた面のまま（ドームが床に乗ったまま）にし、横の位置だけ寄せる。
   */
  function refineCenter() {
    // 続きを撮るときの中心は前回の3Dデータで決まっているので、動かさない。
    if (!center || !dome || reference) return;
    const found = convergePoint(samples);
    if (!found) return;
    // 視線はいまの AR の座標なので、置いたときの座標に戻してから寄せる。
    const back = new THREE.Vector3(...found).applyMatrix4(correction.clone().invert());
    const p: Vec3 = [back.x, back.y, back.z];
    const next: Vec3 = [
      center[0] + (p[0] - center[0]) * refineRate,
      placedInAir ? center[1] + (p[1] - center[1]) * refineRate : center[1],
      center[2] + (p[2] - center[2]) * refineRate,
    ];
    if (Math.hypot(next[0] - center[0], next[1] - center[1], next[2] - center[2]) < 0.005) return;
    center = next;
    dome.group.position.set(...next);
    // ドームの大きさも、回っているときの対象までの距離に合わせる。
    const ds = samples.map((s) => Math.hypot(s.pos[0] - next[0], s.pos[2] - next[2])).sort((a, b) => a - b);
    dome.group.scale.setScalar(domeRadius(ds[Math.floor(ds.length / 2)]!) / domeBaseRadius);
    coverageOf(samples, centerNow()!).forEach((c, i) => (covered[i] = c));
  }

  /** 地図を AR に重ねる: ドームを前回の被写体の中心に置き、前回撮った方向を緑にし、メッシュを見せる。 */
  function applyAlignment(matrix: THREE.Matrix4) {
    const info = reference!.info;
    const toAr = (p: readonly number[]): Vec3 => {
      const v = new THREE.Vector3(p[0], p[1], p[2]).applyMatrix4(matrix);
      return [v.x, v.y, v.z];
    };
    mapGroup.matrix.copy(matrix);
    mapGroup.visible = meshShown;
    center = toAr(info.center);
    const cams = info.cameraPositions.map(toAr);
    const ds = cams.map((c) => Math.hypot(c[0] - center![0], c[2] - center![2])).sort((a, b) => a - b);
    domeBaseRadius = domeRadius(ds.length > 0 ? ds[Math.floor(ds.length / 2)]! : 0.5);
    dome = makeDome(domeBaseRadius);
    // 重ねた所（前回の被写体の中心）のアンカーに貼り付ける。
    keeper.anchor(new THREE.Vector3(...center));
    correction = new THREE.Matrix4();
    dome.group.position.set(...center);
    anchored.add(dome.group);
    // 前回のカメラは被写体を向いて撮っていたので、いる方向のタイルを撮れたことにする。
    for (const c of cams) {
      const { az: a, el } = directionOf(center, c);
      const i = tileAt(a, el);
      if (i !== null) covered[i] = true;
    }
    phase = 'ready';
    emit(performance.now(), true);
  }

  session.addEventListener('end', () => {
    ended = true;
    locator?.dispose();
    keeper.clear();
    disposeMesh();
    // 停止ボタン以外（戻るボタンなど）で閉じたときは、撮りかけの写真を捨てる。
    if (phase !== 'finishing') void collector.discard();
    renderer.setAnimationLoop(null);
    placer.dispose();
    grab.dispose();
    dome?.dispose();
    renderer.dispose();
    opts.onEnded();
  });


  renderer.setAnimationLoop((now: number, frame?: XRFrame) => {
    if (!frame) return;
    const pose = frame.getViewerPose(refSpace);
    // emulatedPosition: AR が位置を見失い、向きしか分かっていない（位置は推測）。置く・数える・照合をしない。
    tracking = !!pose && !pose.emulatedPosition;
    if (!pose || pose.emulatedPosition) {
      if (phase === 'placing') placer.update(frame, refSpace, null, now);
      locator?.update(now, null, null);
      if (pose) renderer.render(scene, camera);
      emit(now);
      return;
    }
    correction = keeper.update(frame, refSpace).clone();
    anchored.matrix.copy(correction);
    const view = pose.views[0]!;
    const t = view.transform;
    pos.set(t.position.x, t.position.y, t.position.z);
    quat.set(t.orientation.x, t.orientation.y, t.orientation.z, t.orientation.w);
    fwd.set(0, 0, -1).applyQuaternion(quat);
    const p: Vec3 = [pos.x, pos.y, pos.z];
    const f: Vec3 = [fwd.x, fwd.y, fwd.z];

    if (phase === 'placing') placer.update(frame, refSpace, { pos, quat, fwd }, now);

    let here: number | null = null;
    const c = centerNow();
    if (c) {
      here = currentTile(p, f, c);
      facing = here !== null;
      az = directionOf(c, p).az;
      if (phase === 'recording') {
        if (here !== null) covered[here] = true;
        if (now - lastSample >= sampleIntervalMs) {
          lastSample = now;
          samples.push({ pos: p, fwd: f });
        }
        if (now - lastRefine >= refineIntervalMs) {
          lastRefine = now;
          refineCenter();
        }
      }
      target = nextTarget(covered, p, c);
    }

    if (phase === 'locating' && locator) {
      const image = cameraImageOf(renderer, view);
      if (!image) noCameraImage = true;
      locator.update(now, { pos, fwd }, image);
      const m = locator.alignment();
      if (m) applyAlignment(m);
    }

    if (phase === 'recording' && sceneMode && now - lastSample >= sampleIntervalMs) {
      lastSample = now;
      warnings.push(now, p, f);
      warn = warnings.warning(now);
    }

    if (phase === 'recording') {
      elapsed = (now - recordStart) / 1000;
      progress = sceneMode ? meter.update(p, f, interval) : elapsed;
      const image = cameraImageOf(renderer, view);
      if (!image) {
        noCameraImage = true;
      } else if (now - lastScore >= scoreIntervalMs) {
        lastScore = now;
        const { tex } = image;
        const score = grab.score(tex, image.width, image.height);
        const r = picker.offer(progress - stageStart, score);
        // 先に前の区間の候補を読み出してから、新しい候補で上書きする。
        closeCandidate(r.closed.map(key));
        if (r.keep) {
          grab.keepCandidate(tex, image.width, image.height, sceneMode ? sceneEncoding.maxEdge : photoEdge);
          candidate = {
            key: key(r.window),
            capturedAt: new Date().toISOString(),
            pose: {
              session: arSessionId,
              position: [pos.x, pos.y, pos.z],
              orientation: [quat.x, quat.y, quat.z, quat.w],
            },
          };
        }
      }
    }

    dome?.paint(covered, here, target?.tile ?? null, now);
    renderer.render(scene, camera);
    emit(now);
  });

  emit(performance.now(), true);

  return {
    record() {
      if (phase !== 'ready') return;
      phase = 'recording';
      recordStart = performance.now();
      emit(recordStart, true);
    },
    async stop() {
      if (phase !== 'recording') {
        if (!ended) await session.end().catch(() => {});
        return [];
      }
      phase = 'finishing';
      emit(performance.now(), true);
      closeCandidate(picker.finish().map(key));
      await collector.settled();
      let frames = collector.sorted();
      while (frames.length > room && room >= 2) {
        const { kept, dropped } = thinPairs(frames);
        dropped.forEach(releaseFrame);
        frames = kept;
      }
      if (!ended) await session.end().catch(() => {});
      return frames;
    },
    cancel() {
      void collector.discard();
      if (!ended) void session.end().catch(() => {});
    },
    toggleMesh() {
      meshShown = !meshShown;
      mapGroup.visible = meshShown && phase !== 'locating';
      emit(performance.now(), true);
    },
    replace() {
      if (phase !== 'ready' || reference) return;
      if (dome) {
        anchored.remove(dome.group);
        dome.dispose();
        dome = null;
      }
      keeper.clear();
      correction = new THREE.Matrix4();
      center = null;
      az = null;
      covered.fill(false);
      target = null;
      placer.hide();
      phase = 'placing';
      emit(performance.now(), true);
    },
  };
}

// ---------------------------------------------------------------------------
// 3D の表示（照準とドーム）
// ---------------------------------------------------------------------------

/** 風景モード: 撮った写真の位置と向き（真上から見たもの）。 */
function pathOf(frames: readonly Frame[]): PathPoint[] {
  const out: PathPoint[] = [];
  for (const f of frames) {
    const a = f.arPose;
    if (!a) continue;
    const q = new THREE.Quaternion(...a.orientation);
    const v = new THREE.Vector3(0, 0, -1).applyQuaternion(q);
    out.push({ x: a.position[0], z: a.position[2], yaw: Math.atan2(v.x, -v.z) });
  }
  return out;
}

/** ドームの半径: 対象までの水平の距離の 4 割（小さすぎ・大きすぎは丸める）。 */
function domeRadius(distance: number): number {
  return Math.min(1.0, Math.max(0.12, distance * 0.4));
}

interface Dome {
  group: THREE.Group;
  paint: (covered: readonly boolean[], here: number | null, target: number | null, now: number) => void;
  dispose: () => void;
}

const WHITE = new THREE.Color(0xffffff);
const GREEN = new THREE.Color(0x34d399);
const AMBER = new THREE.Color(0xf5a524);

/** タイルに分けた半球。撮れた方向は緑、次に撮る方向は琥珀色（点滅）、いま撮っている方向は明るく。 */
function makeDome(radius: number): Dome {
  const group = new THREE.Group();
  const meshes = tiles.map((t) => {
    // 見た目は面より下に潜らないよう、横の帯も 0° から描く。タイルどうしの間に少し隙間を空ける。
    const geometry = patch(radius, t.azFrom + 1.5, t.azTo - 1.5, Math.max(0, t.elFrom) + 1.5, t.elTo - 1.5);
    const material = new THREE.MeshBasicMaterial({
      color: WHITE,
      transparent: true,
      opacity: 0.12,
      side: THREE.DoubleSide,
      depthWrite: false,
    });
    const mesh = new THREE.Mesh(geometry, material);
    group.add(mesh);
    return mesh;
  });
  // 床に置く輪（ドームの縁）。
  const base = new THREE.Mesh(
    new THREE.RingGeometry(radius * 0.98, radius, 64).rotateX(-Math.PI / 2),
    new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.6, depthWrite: false }),
  );
  group.add(base);

  return {
    group,
    paint(covered, here, target, now) {
      const pulse = 0.5 + 0.5 * Math.sin(now / 180);
      meshes.forEach((m, i) => {
        const mat = m.material;
        if (covered[i]) {
          mat.color.copy(GREEN);
          mat.opacity = i === here ? 0.75 : 0.5;
        } else if (i === target) {
          mat.color.copy(AMBER);
          mat.opacity = 0.25 + 0.4 * pulse;
        } else {
          mat.color.copy(WHITE);
          mat.opacity = i === here ? 0.4 : 0.12;
        }
      });
    },
    dispose() {
      for (const m of meshes) {
        m.geometry.dispose();
        m.material.dispose();
      }
      base.geometry.dispose();
      base.material.dispose();
    },
  };
}

/** 球面の一部（方位角・仰角の範囲、度）。方位角は coverage.ts と同じく atan2(x, z)。 */
function patch(radius: number, az0: number, az1: number, el0: number, el1: number): THREE.BufferGeometry {
  const seg = 6;
  const positions: number[] = [];
  const index: number[] = [];
  for (let j = 0; j <= seg; j++) {
    const el = ((el0 + ((el1 - el0) * j) / seg) * Math.PI) / 180;
    for (let i = 0; i <= seg; i++) {
      const az = ((az0 + ((az1 - az0) * i) / seg) * Math.PI) / 180;
      positions.push(radius * Math.cos(el) * Math.sin(az), radius * Math.sin(el), radius * Math.cos(el) * Math.cos(az));
    }
  }
  for (let j = 0; j < seg; j++) {
    for (let i = 0; i < seg; i++) {
      const a = j * (seg + 1) + i;
      const b = a + seg + 1;
      index.push(a, b, a + 1, a + 1, b, b + 1);
    }
  }
  const g = new THREE.BufferGeometry();
  g.setAttribute('position', new THREE.Float32BufferAttribute(positions, 3));
  g.setIndex(index);
  return g;
}

/**
 * 前回のメッシュ（GLB）を読み込み、重ねて見せる用の半透明の材質にする（地図の座標のまま）。
 * 背景の膜が画面を覆わないよう、撮ったカメラの輪の内側だけを残す（viewer/arModel.ts の cropMesh）。
 * 読み込めなければ null（重ねないだけで、撮影は続けられる）。GLTFLoader は続きを撮るときだけ読み込む。
 */
async function loadOverlayMesh(url: string, info: SceneInfo): Promise<{ object: THREE.Object3D; dispose: () => void } | null> {
  try {
    const { GLTFLoader } = await import('three/addons/loaders/GLTFLoader.js');
    const gltf = await new GLTFLoader().loadAsync(url);
    const center = new THREE.Vector3(...info.center);
    const orbit = cameraDistance(info, center);
    if (orbit) cropMesh(gltf.scene, center, keepFraction * orbit);
    const disposables: { dispose: () => void }[] = [];
    gltf.scene.traverse((obj) => {
      const mesh = obj as THREE.Mesh;
      if (!mesh.isMesh) return;
      const source = (Array.isArray(mesh.material) ? mesh.material[0] : mesh.material) as THREE.MeshStandardMaterial;
      const material = new THREE.MeshBasicMaterial({
        map: source?.map ?? null,
        color: source?.map ? 0xffffff : 0xb8b8b8,
        vertexColors: source?.vertexColors ?? false,
        side: THREE.DoubleSide,
        transparent: true,
        opacity: 0.55,
        depthWrite: false,
      });
      for (const m of Array.isArray(mesh.material) ? mesh.material : [mesh.material]) m.dispose();
      mesh.material = material;
      disposables.push(mesh.geometry, material);
      if (material.map) disposables.push(material.map);
    });
    return { object: gltf.scene, dispose: () => disposables.forEach((d) => d.dispose()) };
  } catch {
    return null;
  }
}
