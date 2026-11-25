// AR で置いた物を、現実に貼り付けておく（WebXR の anchors）。
//
// なぜ要るのか: AR（ARCore）は動きながら自分の位置の推定を直し続け、ときどき座標ごと大きく修正する
// （動き始めの数秒、模様の少ない所、見失った後に思い出したとき）。置いた物を「AR を始めた所を原点とする座標」で
// 固定していると、その修正のたびに物が流れて見える（実機で「置いた瞬間どこかへ逃げていく」）。
// アンカーは「この現実の場所」を ARCore に覚えてもらう仕組みで、修正があってもアンカーの位置は現実に追従する。
//
// 使い方: 物を置いた（重ねた）ときの AR の座標での位置で anchor() を呼ぶ。以後、毎フレーム correction() が返す行列を
// 置いた物のグループに掛ける（置いたときからアンカーが動いた分。アンカーが無い端末では常に単位行列）。
import * as THREE from 'three';

export function createAnchorKeeper() {
  let pending: THREE.Vector3 | null = null;
  let anchor: XRAnchor | null = null;
  /** アンカーを作ったときの位置（この位置からの動きを補正にする）。 */
  const origin = new THREE.Matrix4();
  const originInverse = new THREE.Matrix4();
  const correction = new THREE.Matrix4();
  const now = new THREE.Matrix4();

  return {
    /** at（AR の座標）にアンカーを置く。実際に作るのは次の update（フレームの中でしか作れないため）。 */
    anchor(at: THREE.Vector3) {
      anchor?.delete();
      anchor = null;
      correction.identity();
      pending = at.clone();
    },

    /** 毎フレーム呼ぶ。補正の行列（置いたときからアンカーが動いた分）を返す。 */
    update(frame: XRFrame, refSpace: XRReferenceSpace): THREE.Matrix4 {
      if (pending && typeof frame.createAnchor === 'function') {
        const at = pending;
        pending = null;
        origin.makeTranslation(at.x, at.y, at.z);
        originInverse.copy(origin).invert();
        frame
          .createAnchor(new XRRigidTransform({ x: at.x, y: at.y, z: at.z }), refSpace)
          ?.then((a) => {
            anchor = a;
          })
          .catch(() => {
            // アンカーを作れない（端末が対応していない等）。補正なしで続ける。
          });
      } else if (pending) {
        pending = null;
      }
      if (anchor && frame.trackedAnchors?.has(anchor)) {
        const pose = frame.getPose(anchor.anchorSpace, refSpace);
        if (pose) {
          now.fromArray(pose.transform.matrix);
          correction.multiplyMatrices(now, originInverse);
        }
      }
      return correction;
    },

    clear() {
      anchor?.delete();
      anchor = null;
      pending = null;
      correction.identity();
    },
  };
}

export type AnchorKeeper = ReturnType<typeof createAnchorKeeper>;
