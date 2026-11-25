// AR のカメラ画像（WebXR の camera-access。three.js が毎フレーム ExternalTexture にしてくれる）を取り込む係。
// AR で撮る（arSession.ts）・位置合わせ（arLocator.ts）で共通。
//
// 画像は GPU で描き直してから読み出す:
//   くっきり度  長辺 480px に縮めて同期で読む（小さいので速い）
//   写真        フル解像度の描画先に描いて控え、あとで非同期で読み出す
//   位置合わせ  専用の描画先に描いて、すぐ非同期で読み出す（写真の候補を上書きしない）
// 読み出した画素は上下が逆になる（GL の約束）ので、描くときに上下を反転して、上の行から並ぶようにしておく。
import * as THREE from 'three';
import { fit } from './frames';
import { laplacianVariance, toGray } from './sharpness';

/** くっきり度を測るときの長辺（画素）。動画と同じ。 */
const scoreEdge = 480;
/** 写真の長辺の上限（画素）。アップロードと同じ。 */
export const photoEdge = 2048;

export type Grabber = ReturnType<typeof makeGrabber>;

/** WebXR の camera-access の最小限の型（@types/webxr に無い）。 */
export interface XRCameraLike {
  width: number;
  height: number;
}

/** そのフレームのカメラ画像（取れなければ null）。 */
export function cameraImageOf(renderer: THREE.WebGLRenderer, view: XRView): { tex: THREE.Texture; width: number; height: number } | null {
  const xrCamera = (view as XRView & { camera?: XRCameraLike }).camera;
  if (!xrCamera) return null;
  const tex = renderer.xr.getCameraTexture(xrCamera as unknown as Parameters<typeof renderer.xr.getCameraTexture>[0]);
  return tex ? { tex, width: xrCamera.width, height: xrCamera.height } : null;
}


/** カメラ画像（ExternalTexture）を描き直して読み出す係。 */
export function makeGrabber(renderer: THREE.WebGLRenderer) {
  // 上下を反転して描く（読み出した画素が、画像の上の行から並ぶように）。
  const material = new THREE.ShaderMaterial({
    uniforms: { tex: { value: null } },
    vertexShader: 'varying vec2 vUv; void main() { vUv = uv; gl_Position = vec4(position.xy, 0.0, 1.0); }',
    fragmentShader:
      'uniform sampler2D tex; varying vec2 vUv; void main() { gl_FragColor = texture2D(tex, vec2(vUv.x, 1.0 - vUv.y)); }',
    depthTest: false,
    depthWrite: false,
  });
  const quad = new THREE.Mesh(new THREE.PlaneGeometry(2, 2), material);
  const quadScene = new THREE.Scene();
  quadScene.add(quad);
  const quadCamera = new THREE.OrthographicCamera(-1, 1, 1, -1, 0, 1);

  let small: THREE.WebGLRenderTarget | null = null;
  let smallPixels = new Uint8Array(0);
  let gray = new Float32Array(0);
  let full: THREE.WebGLRenderTarget | null = null;
  let shot: THREE.WebGLRenderTarget | null = null;

  const ensure = (rt: THREE.WebGLRenderTarget | null, w: number, h: number) => {
    if (rt && rt.width === w && rt.height === h) return rt;
    rt?.dispose();
    return new THREE.WebGLRenderTarget(w, h, { depthBuffer: false });
  };

  /**
   * XR の描画を一時的に止めて、描画先 rt にカメラ画像を描く。
   * AR の間、three.js は毎フレーム描画先を AR の画面（XR の framebuffer）にしている。
   * null（ふつうの canvas）に戻すとドームが AR の画面に出なくなるので、元の描画先に戻す。
   */
  const draw = (tex: THREE.Texture, rt: THREE.WebGLRenderTarget) => {
    material.uniforms.tex!.value = tex;
    const previous = renderer.getRenderTarget();
    const xrEnabled = renderer.xr.enabled;
    renderer.xr.enabled = false;
    renderer.setRenderTarget(rt);
    renderer.render(quadScene, quadCamera);
    renderer.xr.enabled = xrEnabled;
    renderer.setRenderTarget(previous);
  };

  return {
    /** くっきり度（長辺 480px に縮めたラプラシアン分散）。 */
    score(tex: THREE.Texture, width: number, height: number): number {
      const s = fit(width, height, scoreEdge);
      small = ensure(small, s.width, s.height);
      if (smallPixels.length !== s.width * s.height * 4) {
        smallPixels = new Uint8Array(s.width * s.height * 4);
        gray = new Float32Array(s.width * s.height);
      }
      draw(tex, small);
      renderer.readRenderTargetPixels(small, 0, 0, s.width, s.height, smallPixels);
      return laplacianVariance(toGray(smallPixels, gray), s.width, s.height);
    },
    /** いまのカメラ画像を、写真の候補として控える（前の候補は上書き）。 */
    keepCandidate(tex: THREE.Texture, width: number, height: number, maxEdge = photoEdge) {
      const s = fit(width, height, maxEdge);
      full = ensure(full, s.width, s.height);
      draw(tex, full);
    },
    /** 控えた候補を読み出して画像にする（読み出しの命令はすぐ出すので、この後に上書きしてよい）。 */
    async readCandidate(): Promise<ImageBitmap> {
      const rt = full;
      if (!rt) throw new Error('候補がありません');
      const pixels = new Uint8Array(rt.width * rt.height * 4);
      await renderer.readRenderTargetPixelsAsync(rt, 0, 0, rt.width, rt.height, pixels);
      return createImageBitmap(new ImageData(new Uint8ClampedArray(pixels.buffer), rt.width, rt.height));
    },
    /** いまのカメラ画像を、写真の候補とは別に読み出す（位置合わせに送る 1 枚）。 */
    async snapshot(tex: THREE.Texture, width: number, height: number, maxEdge = photoEdge): Promise<ImageBitmap> {
      const s = fit(width, height, maxEdge);
      shot = ensure(shot, s.width, s.height);
      draw(tex, shot);
      const rt = shot;
      const pixels = new Uint8Array(rt.width * rt.height * 4);
      await renderer.readRenderTargetPixelsAsync(rt, 0, 0, rt.width, rt.height, pixels);
      return createImageBitmap(new ImageData(new Uint8ClampedArray(pixels.buffer), rt.width, rt.height));
    },
    dispose() {
      small?.dispose();
      full?.dispose();
      shot?.dispose();
      quad.geometry.dispose();
      material.dispose();
    },
  };
}
