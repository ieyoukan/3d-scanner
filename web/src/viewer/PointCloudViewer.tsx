// SfM の結果（疎な点群とカメラ位置）のビューア。
//
// メッシュや 3DGS が出来なかったときでも、SfM が成功していればこれは見られる。
// 「写真のどこが 3D の点になったか」「カメラがどこにあったと推定されたか」を確かめられるので、
// 撮り方の振り返り（どの方向の写真が足りなかったか）にも使える。
import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import * as THREE from 'three';
import { PLYLoader } from 'three/addons/loaders/PLYLoader.js';
import type { SceneInfo } from '../api/types';
import type { ViewFrame } from './sceneFrame';
import { createStage, type Stage } from './threeStage';
import { describeViewerError, type ViewerHandle, type ViewerState } from './types';

interface Props {
  url: string;
  frame: ViewFrame;
  info: SceneInfo | null;
  onState: (state: ViewerState) => void;
}

export const PointCloudViewer = forwardRef<ViewerHandle, Props>(function PointCloudViewer(
  { url, frame, info, onState },
  ref,
) {
  const container = useRef<HTMLDivElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const stage = useRef<Stage | null>(null);

  useImperativeHandle(ref, () => ({ resetView: () => stage.current?.reset() }), []);

  useEffect(() => {
    if (!container.current || !canvas.current) return;
    let disposed = false;
    let current: Stage;
    try {
      current = createStage(canvas.current, container.current, frame);
    } catch (err) {
      onState({ kind: 'error', message: describeViewerError(err) });
      return;
    }
    stage.current = current;
    onState({ kind: 'loading' });

    // カメラ位置を琥珀色の点で表示する（点群より大きく描いて区別する）。
    if (info && info.cameraPositions.length > 0) {
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute('position', new THREE.Float32BufferAttribute(info.cameraPositions.flat(), 3));
      const material = new THREE.PointsMaterial({ color: 0xf2a93b, size: 9, sizeAttenuation: false });
      current.content.add(new THREE.Points(geometry, material));
    }

    new PLYLoader().load(
      url,
      (geometry) => {
        if (disposed) {
          geometry.dispose();
          return;
        }
        // COLMAP が書き出す PLY は、点ごとに色（写真から取った色）を持っている。
        // sizeAttenuation: false は「遠くの点も同じ大きさで描く」指定。点がまばらなので、その方が見やすい。
        const material = new THREE.PointsMaterial({
          size: 2.5,
          sizeAttenuation: false,
          vertexColors: geometry.hasAttribute('color'),
        });
        const points = new THREE.Points(geometry, material);
        current.content.add(points);
        if (frame.approximate) current.fitTo(points);
        const count = geometry.attributes.position?.count ?? 0;
        const cameras = info ? `・カメラ ${info.registeredImages} 台` : '';
        onState({ kind: 'ready', summary: `点 ${count.toLocaleString()} 個${cameras}` });
      },
      undefined,
      (err) => {
        if (!disposed) onState({ kind: 'error', message: describeViewerError(err) });
      },
    );

    return () => {
      disposed = true;
      stage.current = null;
      current.dispose();
    };
  }, [url, frame, info, onState]);

  return (
    <div ref={container} className="h-full w-full">
      <canvas ref={canvas} className="block h-full w-full touch-none" />
    </div>
  );
});
