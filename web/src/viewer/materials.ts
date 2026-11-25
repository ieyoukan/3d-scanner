// メッシュの材質。MeshViewer（ふつうの表示）と arView（AR で見る）で共通。
import * as THREE from 'three';

/**
 * 材質を「光の影響を受けない」ものに置き換える。
 *
 * 写真から作ったテクスチャには、撮影時の光と影が既に写り込んでいる。
 * ここにさらに 3D の照明計算を掛けると、影が二重になって不自然に暗くなる。
 * MeshBasicMaterial はテクスチャの色をそのまま表示する材質。
 * side: DoubleSide は面の裏側も描く指定。簡易メッシュは面の向きが揃っていないことがあるため。
 */
export function unlit(material: THREE.Material | THREE.Material[]): THREE.Material {
  const source = (Array.isArray(material) ? material[0] : material) as THREE.MeshStandardMaterial | undefined;
  const basic = new THREE.MeshBasicMaterial({
    map: source?.map ?? null,
    color: source?.map ? 0xffffff : (source?.color ?? new THREE.Color(0xb8b8b8)),
    vertexColors: source?.vertexColors ?? false,
    side: THREE.DoubleSide,
  });
  // 元の材質はもう使わないので解放する（テクスチャは引き継いだので解放しない）。
  for (const m of Array.isArray(material) ? material : [material]) m.dispose();
  return basic;
}
