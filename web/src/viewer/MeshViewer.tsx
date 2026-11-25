// メッシュ（GLB）のビューア。three.js の GLTFLoader で読み込む。
import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react';
import * as THREE from 'three';
import { GLTFLoader } from 'three/addons/loaders/GLTFLoader.js';
import { unlit } from './materials';
import type { ViewFrame } from './sceneFrame';
import { createStage, type Stage } from './threeStage';
import { describeViewerError, type ViewerHandle, type ViewerState } from './types';

interface Props {
  url: string;
  frame: ViewFrame;
  onState: (state: ViewerState) => void;
}

// forwardRef + useImperativeHandle は、親から「視点を戻して」と頼めるようにする仕組み。
// React では普通、親→子の指示は props で伝えるが、「いま1回だけ実行して」という命令は
// props では表しにくい。そういうときに、子が関数（resetView）を親に貸し出す。
export const MeshViewer = forwardRef<ViewerHandle, Props>(function MeshViewer({ url, frame, onState }, ref) {
  const container = useRef<HTMLDivElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const stage = useRef<Stage | null>(null);

  useImperativeHandle(ref, () => ({ resetView: () => stage.current?.reset() }), []);

  useEffect(() => {
    if (!container.current || !canvas.current) return;
    // この effect が後片付けされた後に読み込みが終わっても、結果を使わないための印。
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

    new GLTFLoader().load(
      url,
      (gltf) => {
        if (disposed) return;
        let triangles = 0;
        gltf.scene.traverse((obj) => {
          const mesh = obj as THREE.Mesh;
          if (!mesh.isMesh) return;
          const index = mesh.geometry.index;
          triangles += (index ? index.count : mesh.geometry.attributes.position?.count ?? 0) / 3;
          mesh.material = unlit(mesh.material);
        });
        current.content.add(gltf.scene);
        if (frame.approximate) current.fitTo(gltf.scene);
        onState({ kind: 'ready', summary: `三角形 ${Math.round(triangles).toLocaleString()} 枚` });
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
  }, [url, frame, onState]);

  return (
    <div ref={container} className="h-full w-full">
      {/* touch-none: 指の動きをブラウザのスクロールやズームに使わせず、3D操作にだけ使う。 */}
      <canvas ref={canvas} className="block h-full w-full touch-none" />
    </div>
  );
});
