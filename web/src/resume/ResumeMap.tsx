// 前回の撮影位置と、位置合わせ（VPS）で分かった今回の位置を、真上から見た地図に描く。
//
//   灰色の点  前回、写真を撮った位置（scene.json の cameraPositions）
//   十字      被写体の中心
//   琥珀色    今回、位置が分かった写真。最新のものは大きく描き、向いている方向を扇形で示す
//
// 座標の変換は mapView.ts（テスト済みの純粋な関数）が行う。ここは描くだけ。
import { useMemo } from 'react';
import type { SceneInfo } from '../api/types';
import { buildMapFrame, fitView, headingOnMap, toMap, type MapPoint } from './mapView';
import type { Fix } from './useLocalize';

interface Props {
  info: SceneInfo;
  fixes: Fix[];
  /** 小さな表示（撮影画面の隅）。凡例を出さず、点を少し大きめにする。 */
  compact?: boolean;
  className?: string;
}

export function ResumeMap({ info, fixes, compact = false, className = '' }: Props) {
  const drawing = useMemo(() => {
    const frame = buildMapFrame(info);
    const cameras = info.cameraPositions.map((p) => toMap(frame, p));
    const placed = fixes.map((f) => ({ id: f.frameId, at: toMap(frame, f.position), heading: headingOnMap(frame, f.forward) }));
    const view = fitView([[0, 0], ...cameras, ...placed.map((p) => p.at)], Math.max(info.radius, 1e-3) * 1.5);
    return { cameras, placed, view };
  }, [info, fixes]);

  const { cameras, placed, view } = drawing;
  // 1単位 = 表示範囲の 1%。点や線の大きさを地図の縮尺によらず一定に見せる。
  const u = view.size / 100;
  const latest = placed.at(-1);

  return (
    <figure className={className}>
      <svg
        viewBox={`${view.minX} ${view.minY} ${view.size} ${view.size}`}
        className="block aspect-square w-full rounded-lg bg-ink"
        role="img"
        aria-label={
          latest
            ? `前回の撮影位置${cameras.length}か所と、今回位置が分かった${placed.length}か所の地図`
            : `前回の撮影位置${cameras.length}か所の地図`
        }
      >
        {/* 被写体の中心 */}
        <path
          d={`M ${-3 * u} 0 H ${3 * u} M 0 ${-3 * u} V ${3 * u}`}
          className="stroke-mute"
          strokeWidth={0.6 * u}
          strokeLinecap="round"
        />
        {cameras.map(([x, y], i) => (
          <circle key={i} cx={x} cy={y} r={(compact ? 1.8 : 1.2) * u} className="fill-mute" opacity={0.75} />
        ))}
        {placed.slice(0, -1).map((p) => (
          <circle key={p.id} cx={p.at[0]} cy={p.at[1]} r={(compact ? 2.2 : 1.6) * u} className="fill-amber" opacity={0.55} />
        ))}
        {latest && (
          <g>
            {latest.heading && <path d={cone(latest.at, latest.heading, 16 * u)} className="fill-amber" opacity={0.3} />}
            <circle cx={latest.at[0]} cy={latest.at[1]} r={4.2 * u} className="fill-none stroke-amber" strokeWidth={0.7 * u} />
            <circle cx={latest.at[0]} cy={latest.at[1]} r={2.4 * u} className="fill-amber" />
          </g>
        )}
      </svg>
      {!compact && (
        <figcaption className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 text-xs text-mute">
          <span className="flex items-center gap-1.5">
            <span className="inline-block size-2 rounded-full bg-mute" />
            前回の撮影位置
          </span>
          <span className="flex items-center gap-1.5">
            <span className="inline-block size-2 rounded-full bg-amber" />
            今回、位置が分かった写真
          </span>
          <span className="flex items-center gap-1.5">
            <span aria-hidden="true">＋</span>
            被写体の中心
          </span>
        </figcaption>
      )}
    </figure>
  );
}

/** 向いている方向を示す扇形（左右 25 度、長さ length）。 */
function cone(at: MapPoint, heading: MapPoint, length: number): string {
  const spread = (25 * Math.PI) / 180;
  const rotate = ([x, y]: MapPoint, a: number): MapPoint => [x * Math.cos(a) - y * Math.sin(a), x * Math.sin(a) + y * Math.cos(a)];
  const l = rotate(heading, -spread);
  const r = rotate(heading, spread);
  const p = (v: MapPoint) => `${at[0] + v[0] * length} ${at[1] + v[1] * length}`;
  return `M ${at[0]} ${at[1]} L ${p(l)} A ${length} ${length} 0 0 1 ${p(r)} Z`;
}
