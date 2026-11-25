// AR で「どこに置くか」を決める照準（輪）。AR で撮る（arSession.ts）と AR で見る（viewer/arView.ts）で共通。
//
// 輪の出し方:
//   面や特徴点に当たったとき  その面に寝かせた、くっきりした白い輪
//   何にも当たらないとき      1.5 秒待ってから、前方の空中（前に当たった距離、無ければ 0.6 m 先）に、
//                             こちらを向いた薄い輪を出す（白い机など、面を見つけにくい所でも置けるように）
// hit test は面と特徴点（対象の表面など）に当てる。特徴点に対応しない端末では面だけ。
import * as THREE from 'three';

/** 面も特徴点も見つからないとき、空中に照準を出すまで待つ時間（ミリ秒）。 */
const airAfterMs = 1500;
/** 空中に照準を出す距離（前に何かに当たっていればその距離）。 */
const airDistance = 0.6;

export interface Placer {
  /** 輪（シーンに足す）。 */
  readonly group: THREE.Group;
  /** 輪が出ている（タップで置ける）。 */
  readonly aiming: boolean;
  /** 輪が空中にある（面や特徴点に当たっていない）。 */
  readonly inAir: boolean;
  /** 毎フレーム呼ぶ。pose が無い（端末の位置が分からない）ときは null を渡す。 */
  update(frame: XRFrame, refSpace: XRReferenceSpace, camera: CameraPose | null, now: number): void;
  /** 輪の位置。 */
  position(): THREE.Vector3;
  /** 輪を隠し、空中に出すまでの待ちを最初からにする（置いた後・置き直すとき）。 */
  hide(): void;
  dispose(): void;
}

/** そのフレームのカメラの位置・向き・視線。 */
export interface CameraPose {
  pos: THREE.Vector3;
  quat: THREE.Quaternion;
  fwd: THREE.Vector3;
}

/** 照準を作る。session は hit test を頼むのに使う。 */
export async function createPlacer(session: XRSession): Promise<Placer> {
  const viewerSpace = await session.requestReferenceSpace('viewer');
  const hitSource = await requestHitTest(session, viewerSpace);
  const reticle = makeReticle();
  let aiming = false;
  let noHitSince: number | null = null;
  let lastHitDistance = airDistance;

  return {
    group: reticle.group,
    get aiming() {
      return aiming;
    },
    get inAir() {
      return reticle.inAir;
    },
    update(frame, refSpace, camera, now) {
      if (!camera) {
        // 位置が分からなくなった（端末を速く動かした・暗い等）。照準は出さない。
        aiming = false;
        reticle.group.visible = false;
        noHitSince = null;
        return;
      }
      const hit = hitSource ? frame.getHitTestResults(hitSource)[0] : undefined;
      const hitPose = hit?.getPose(refSpace);
      if (hitPose) {
        noHitSince = null;
        const m = hitPose.transform.position;
        // 遠くの壁などに当たったときに空中の照準が遠くへ行きすぎないよう、0.3〜1.5 m に収める。
        const d = Math.hypot(m.x - camera.pos.x, m.y - camera.pos.y, m.z - camera.pos.z);
        lastHitDistance = Math.min(1.5, Math.max(0.3, d));
        reticle.onSurface(hitPose.transform.matrix);
        aiming = true;
      } else {
        noHitSince ??= now;
        aiming = now - noHitSince >= airAfterMs;
        if (aiming) reticle.inFront(camera.pos, camera.quat, camera.fwd, lastHitDistance);
        else reticle.group.visible = false;
      }
    },
    position() {
      return new THREE.Vector3().setFromMatrixPosition(reticle.group.matrix);
    },
    hide() {
      aiming = false;
      reticle.group.visible = false;
      noHitSince = null;
    },
    dispose() {
      hitSource?.cancel();
      reticle.dispose();
    },
  };
}

/** 面と特徴点に当てる hit test を頼む。特徴点に対応していなければ面だけ。どちらも駄目なら undefined。 */
async function requestHitTest(session: XRSession, space: XRReferenceSpace): Promise<XRHitTestSource | undefined> {
  const request = session.requestHitTestSource?.bind(session);
  if (!request) return undefined;
  try {
    return await request({ space, entityTypes: ['plane', 'point'] });
  } catch {
    try {
      return await request({ space });
    } catch {
      return undefined;
    }
  }
}

function makeReticle() {
  const group = new THREE.Group();
  group.matrixAutoUpdate = false;
  group.visible = false;
  const ring = (inner: number, outer: number) => new THREE.RingGeometry(inner, outer, 40);
  const surface = new THREE.Mesh(
    ring(0.05, 0.065).rotateX(-Math.PI / 2),
    new THREE.MeshBasicMaterial({ color: 0xffffff }),
  );
  const air = new THREE.Mesh(
    ring(0.045, 0.055),
    new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.55, depthWrite: false }),
  );
  group.add(surface, air);
  const m = new THREE.Matrix4();
  const at = new THREE.Vector3();
  const one = new THREE.Vector3(1, 1, 1);
  return {
    group,
    inAir: false,
    onSurface(matrix: Float32Array) {
      group.matrix.fromArray(matrix);
      surface.visible = true;
      air.visible = false;
      group.visible = true;
      this.inAir = false;
    },
    inFront(pos: THREE.Vector3, quat: THREE.Quaternion, fwd: THREE.Vector3, distance: number) {
      at.copy(fwd).multiplyScalar(distance).add(pos);
      group.matrix.copy(m.compose(at, quat, one));
      surface.visible = false;
      air.visible = true;
      group.visible = true;
      this.inAir = true;
    },
    dispose() {
      surface.geometry.dispose();
      surface.material.dispose();
      air.geometry.dispose();
      air.material.dispose();
    },
  };
}
