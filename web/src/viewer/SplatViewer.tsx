// 3D Gaussian Splatting（SOG / PLY）のビューア。PlayCanvas のエンジンを使う。
//
// 3DGS はメッシュ（三角形の面）ではなく、数万〜数百万個の「半透明の色つきの粒」の集まり。
// 正しく描くには、粒を毎フレーム奥から手前へ並べ替えて重ね塗りする専用の仕組みが要る。
// それを自作せず、PlayCanvas が公開している実装（gsplat コンポーネント）に任せる。
//
// PlayCanvas の基本的な考え方（three.js との違い）:
//   Application  全体の管理者（描画ループも持つ）
//   Entity       シーンに置く「もの」。位置・回転を持つ
//   Component    Entity に付ける機能（camera = カメラ、gsplat = スプラット、script = 振る舞い）
//   Asset        読み込むファイル
import * as pc from 'playcanvas';
import { CameraControls } from 'playcanvas/scripts/esm/camera-controls.mjs';
import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import type { ViewFrame } from './sceneFrame';
import { baseFov, verticalFov } from './threeStage';
import { describeViewerError, type ViewerHandle, type ViewerState } from './types';

interface Props {
  /** .sog か .ply の URL。PlayCanvas は URL の拡張子で形式を見分ける。 */
  url: string;
  frame: ViewFrame;
  onState: (state: ViewerState) => void;
}

export const SplatViewer = forwardRef<ViewerHandle, Props>(function SplatViewer({ url, frame, onState }, ref) {
  const container = useRef<HTMLDivElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  // 親から呼ばれる「視点を戻す」処理。初期化が済んだら中身を入れる。
  const reset = useRef<() => void>(() => {});

  useImperativeHandle(ref, () => ({ resetView: () => reset.current() }), []);

  useEffect(() => {
    const host = container.current;
    const target = canvas.current;
    if (!host || !target) return;
    let disposed = false;
    let app: pc.Application;
    try {
      app = new pc.Application(target, {
        // 入力（マウス・タッチ）は CameraControls が自分で canvas から受け取るので、ここでは渡さない。
        graphicsDeviceOptions: { antialias: false, alpha: false },
      });
    } catch (err) {
      onState({ kind: 'error', message: describeViewerError(err) });
      return;
    }
    onState({ kind: 'loading' });

    // canvas の大きさは自分で管理する（下の ResizeObserver）。
    app.setCanvasFillMode(pc.FILLMODE_NONE);
    app.setCanvasResolution(pc.RESOLUTION_AUTO);
    // 高精細な画面でも描く画素数を2倍までに抑える（スプラットは画素数に比例して重くなる）。
    app.graphicsDevice.maxPixelRatio = Math.min(window.devicePixelRatio, 2);

    // カメラ。背景色は styles.css の --color-ink と同じ。
    const camera = new pc.Entity('camera');
    camera.addComponent('camera', {
      clearColor: new pc.Color(15 / 255, 16 / 255, 17 / 255),
      fov: baseFov,
      nearClip: frame.radius / 200,
      farClip: frame.radius * 200,
    });

    const resize = () => {
      const { clientWidth: w, clientHeight: h } = host;
      if (w === 0 || h === 0) return;
      app.resizeCanvas(w, h);
      // メッシュのビューアと同じ見え方にする（縦長の画面では画角を広げる）。
      if (camera.camera) camera.camera.fov = verticalFov(w / h);
    };
    const observer = new ResizeObserver(resize);
    observer.observe(host);
    resize();
    const home = new pc.Vec3(...frame.cameraPosition);
    camera.setPosition(home);
    camera.lookAt(pc.Vec3.ZERO);
    app.root.addChild(camera);

    // マウス・タッチでの回転／移動／拡大。注視点は原点（＝被写体の中心）。
    camera.addComponent('script');
    const controls = camera.script?.create(CameraControls, {
      properties: {
        focusPoint: new pc.Vec3(0, 0, 0),
        // 飛び回る操作は、慣れないと迷子になりやすいので無効にする。
        enableFly: false,
        zoomRange: new pc.Vec2(frame.radius * 0.05, frame.radius * 40),
      },
    }) as CameraControls | undefined;
    reset.current = () => controls?.reset(new pc.Vec3(0, 0, 0), home);

    // スプラット本体。メッシュのビューアと同じ回転・平行移動を掛ける（タブを切り替えても向きが揃う）。
    const splat = new pc.Entity('splat');
    splat.setLocalRotation(new pc.Quat(...frame.rotation));
    splat.setLocalPosition(...frame.position);
    app.root.addChild(splat);

    const asset = new pc.Asset('scan', 'gsplat', { url });
    asset.once('load', () => {
      if (disposed) return;
      splat.addComponent('gsplat', { asset });
      onState({ kind: 'ready', summary: describeSplat(asset) });
    });
    asset.once('error', (err: unknown) => {
      if (!disposed) onState({ kind: 'error', message: describeViewerError(err) });
    });
    app.assets.add(asset);
    app.assets.load(asset);

    app.start();

    return () => {
      disposed = true;
      reset.current = () => {};
      observer.disconnect();
      // destroy は描画ループを止め、読み込んだデータと GPU 上の資源をまとめて解放する。
      app.destroy();
    };
  }, [url, frame, onState]);

  return (
    <div ref={container} className="h-full w-full">
      <canvas ref={canvas} className="block h-full w-full touch-none" />
    </div>
  );
});

/** 読み込んだスプラットの粒の数を、分かる範囲で言葉にする。 */
function describeSplat(asset: pc.Asset): string {
  // 粒の数の持ち方は形式（PLY / SOG）によって違うので、あれば使う、という扱いにする。
  const resource = asset.resource as { gsplatData?: { numSplats?: number }; numSplats?: number } | undefined;
  const count = resource?.gsplatData?.numSplats ?? resource?.numSplats;
  return typeof count === 'number' ? `ガウシアン ${count.toLocaleString()} 個` : 'Gaussian Splat';
}
