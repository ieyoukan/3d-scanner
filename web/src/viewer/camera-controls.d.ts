// PlayCanvas に同梱されているカメラ操作スクリプト（scripts/esm/camera-controls.mjs）の型宣言。
// 本体は JavaScript で配られていて型定義が付いていないので、このアプリが使う部分だけを宣言する。
// 内容は node_modules/playcanvas/scripts/esm/camera-controls.mjs の JSDoc に合わせてある。
declare module 'playcanvas/scripts/esm/camera-controls.mjs' {
  import type { Script, Vec2, Vec3 } from 'playcanvas';

  export class CameraControls extends Script {
    static scriptName: string;
    /** 回転の中心（注視点）。 */
    focusPoint: Vec3;
    /** 注視点のまわりを回る操作を有効にするか。 */
    enableOrbit: boolean;
    /** キーボード等で自由に飛び回る操作を有効にするか。 */
    enableFly: boolean;
    /** 平行移動を有効にするか。 */
    enablePan: boolean;
    /** ズームできる距離の範囲 (最小, 最大)。 */
    zoomRange: Vec2;
    /** 注視点 focus を、位置 position から見る状態にする。 */
    reset(focus: Vec3, position: Vec3): void;
  }
}
