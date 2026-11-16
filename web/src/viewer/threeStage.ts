// three.js の「舞台」づくりをまとめた補助。メッシュのビューアと点群のビューアが共有する。
//
// three.js で何かを表示するには、最低限この4つが要る:
//   Scene     表示する物を入れる入れ物
//   Camera    どこから見るか
//   Renderer  Scene を Camera から見た絵を <canvas> に描く係
//   描画ループ  画面の更新のたびに描き直す繰り返し
// それに加えて、マウスや指での操作（OrbitControls）と、
// 画面サイズが変わったときの追従、使い終わったときの後片付けを用意する。
import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import type { ViewFrame } from './sceneFrame';

export interface Stage {
  scene: THREE.Scene;
  /** モデルを入れるグループ。ViewFrame の回転と平行移動が掛かっている。 */
  content: THREE.Group;
  camera: THREE.PerspectiveCamera;
  /** 視点を初期位置に戻す。 */
  reset: () => void;
  /** 形状の大きさに合わせて視点を取り直す（scene.json が無いときの代替）。 */
  fitTo: (object: THREE.Object3D) => void;
  /** 後片付け。描画ループを止め、GPU 上の資源を解放する。 */
  dispose: () => void;
}

/** 背景色。styles.css の --color-ink と同じ。 */
const background = 0x0f1011;

/** 基準の視野角（度）。横長の画面では縦方向、縦長の画面では横方向の角度として使う。 */
export const baseFov = 50;

/**
 * 画面の縦横比に応じた「縦方向の視野角」を返す。
 *
 * three.js のカメラは縦方向の視野角で指定する。そのままだと、スマホの縦長画面では
 * 横に見える範囲がとても狭くなり、モデルの左右が切れてしまう。
 * 縦長のときは「横方向が baseFov になる」ように縦方向の角度を広げる。
 *   tan(縦/2) = tan(横/2) ÷ 縦横比
 */
export function verticalFov(aspect: number): number {
  if (aspect >= 1) return baseFov;
  const half = Math.atan(Math.tan((baseFov * Math.PI) / 360) / aspect);
  // 広げすぎると端が歪むので上限を置く。
  return Math.min((half * 360) / Math.PI, 85);
}

export function createStage(canvas: HTMLCanvasElement, container: HTMLElement, frame: ViewFrame): Stage {
  const renderer = new THREE.WebGLRenderer({ canvas, antialias: true });
  // 高精細な画面では、CSS の1画素に対して実際の画素が2〜3個ある。それに合わせて描くとくっきりするが、
  // 3倍にすると描く画素が9倍になって重い。2倍までに抑える。
  renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
  renderer.setClearColor(background);

  const scene = new THREE.Scene();
  const content = new THREE.Group();
  content.quaternion.set(...frame.rotation);
  content.position.set(...frame.position);
  scene.add(content);

  // near/far は「これより近い・遠いものは描かない」範囲。
  // 範囲が広すぎると奥行きの精度が落ちてちらつくので、モデルの大きさから決める。
  const camera = new THREE.PerspectiveCamera(baseFov, 1, frame.radius / 200, frame.radius * 200);

  // OrbitControls: 左ドラッグ／1本指で回転、右ドラッグ／2本指で移動、ホイール／ピンチで拡大。
  const controls = new OrbitControls(camera, canvas);
  controls.enableDamping = true; // 指を離した後、少し滑ってから止まる
  controls.dampingFactor = 0.1;
  controls.minDistance = frame.radius * 0.05;
  controls.maxDistance = frame.radius * 40;

  let home = new THREE.Vector3(...frame.cameraPosition);
  const reset = () => {
    camera.position.copy(home);
    controls.target.set(0, 0, 0);
    controls.update();
  };
  reset();

  const fitTo = (object: THREE.Object3D) => {
    const box = new THREE.Box3().setFromObject(object);
    if (box.isEmpty()) return;
    const sphere = box.getBoundingSphere(new THREE.Sphere());
    // 中心が原点に来るよう、入れ物ごとずらす。
    content.position.sub(sphere.center);
    const r = Math.max(sphere.radius, 1e-3);
    camera.near = r / 200;
    camera.far = r * 200;
    camera.updateProjectionMatrix();
    controls.minDistance = r * 0.05;
    controls.maxDistance = r * 40;
    home = new THREE.Vector3(0, r * 0.4, r * 2.5);
    reset();
  };

  // 表示領域の大きさが変わったら（端末の回転、ウィンドウのリサイズ）、描画サイズと縦横比を合わせる。
  const resize = () => {
    const { clientWidth: w, clientHeight: h } = container;
    if (w === 0 || h === 0) return;
    renderer.setSize(w, h, false);
    camera.aspect = w / h;
    camera.fov = verticalFov(w / h);
    camera.updateProjectionMatrix();
  };
  const observer = new ResizeObserver(resize);
  observer.observe(container);
  resize();

  // 描画ループ。setAnimationLoop は画面の更新に合わせて（通常は毎秒60回）関数を呼ぶ。
  renderer.setAnimationLoop(() => {
    controls.update();
    renderer.render(scene, camera);
  });

  const dispose = () => {
    renderer.setAnimationLoop(null);
    observer.disconnect();
    controls.dispose();
    // 形状・材質・テクスチャは GPU のメモリに載っている。JavaScript の変数を捨てるだけでは
    // 解放されないので、1つずつ dispose を呼ぶ。忘れると、ビューアを開閉するたびにメモリが減っていく。
    scene.traverse((obj) => {
      const mesh = obj as THREE.Mesh;
      mesh.geometry?.dispose();
      const materials = Array.isArray(mesh.material) ? mesh.material : mesh.material ? [mesh.material] : [];
      for (const material of materials) {
        for (const value of Object.values(material)) {
          if (value instanceof THREE.Texture) value.dispose();
        }
        material.dispose();
      }
    });
    renderer.dispose();
  };

  return { scene, content, camera, reset, fitTo, dispose };
}
