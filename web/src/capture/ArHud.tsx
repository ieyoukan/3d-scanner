// AR で撮っている間に、カメラ映像の上に重ねる画面（WebXR の DOM overlay）。
//
// 文字はできるだけ使わず、見た目で伝える:
//   上の真ん中  真上から見たドームの小さな図。外の輪が横の 12 方向、内の輪が上の 6 方向、真ん中が真上。
//              撮れた方向は緑、次に撮る方向は琥珀色
//   画面の端    次に撮る方向へ、左・右・上の矢印
//   下          録画ボタン（赤い丸 → 赤い四角）。横の1周が埋まったら緑に光る
//   続きを撮る  最初は位置合わせ中の照準（前回撮った所を映してもらう）。重なったら、前回のメッシュを
//              見せる・隠すボタン（目の形）を右上に出す
import { useEffect, useRef } from 'react';
import { tiles } from './coverage';
import type { ArState } from './arSession';

interface Props {
  state: ArState | null;
  onRecord: () => void;
  onStop: () => void;
  onCancel: () => void;
  onReplace: () => void;
  onToggleMesh: () => void;
  /** 中心を置き直せる（続きを撮るときは、中心は前回の3Dデータで決まるので置き直せない）。 */
  canReplace: boolean;
}

export function ArHud({ state, onRecord, onStop, onCancel, onReplace, onToggleMesh, canReplace }: Props) {
  const root = useRef<HTMLDivElement>(null);

  // ボタンを押したときに、AR の「画面のタップ」（中心を置く）が起きないようにする。
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const block = (e: Event) => {
      if ((e.target as Element | null)?.closest('button')) e.preventDefault();
    };
    el.addEventListener('beforexrselect', block);
    return () => el.removeEventListener('beforexrselect', block);
  }, []);

  const phase = state?.phase ?? 'placing';
  const locating = phase === 'locating';
  const sideDone = state ? tiles.filter((t, i) => t.band === 'side' && state.covered[i]).length : 0;
  const loopDone = sideDone === tiles.filter((t) => t.band === 'side').length;
  const turn = phase === 'recording' || phase === 'ready' ? state?.target?.turn : undefined;

  return (
    <div ref={root} className="pointer-events-none fixed inset-0 z-50 select-none text-paper">
      {/* 上: 閉じる・進み具合・中心の置き直し */}
      <div className="pad-safe-t flex items-start justify-between px-3 pt-2">
        <RoundButton label="閉じる" onClick={onCancel}>
          <path d="M6 6l12 12M18 6L6 18" />
        </RoundButton>
        {state && phase !== 'placing' && !locating && <DomeMap state={state} />}
        {state?.meshShown != null && !locating ? (
          <RoundButton label={state.meshShown ? '前回のメッシュを隠す' : '前回のメッシュを見せる'} onClick={onToggleMesh}>
            <path d="M2 12s3.6-6 10-6 10 6 10 6-3.6 6-10 6S2 12 2 12z" />
            <circle cx="12" cy="12" r="2.5" />
            {!state.meshShown && <path d="M4 20L20 4" />}
          </RoundButton>
        ) : phase === 'ready' && canReplace ? (
          <RoundButton label="中心を置き直す" onClick={onReplace}>
            <circle cx="12" cy="12" r="7" />
            <circle cx="12" cy="12" r="1.5" fill="currentColor" />
          </RoundButton>
        ) : (
          <span className="size-11" />
        )}
      </div>

      {/* 中心を置く段階だけ、短い案内を出す。照準が出るまでは、端末をゆっくり動かす絵 */}
      {phase === 'placing' && (
        <div className="absolute inset-x-0 top-1/2 mt-16 flex flex-col items-center gap-2 drop-shadow">
          {!state?.aiming && <MovePhone />}
          <p className="text-center text-base font-semibold">
            {state?.aiming ? '対象に合わせてタップ' : 'ゆっくり動かす'}
          </p>
        </div>
      )}

      {/* 続きを撮る: 位置合わせ中。前回撮った所を映してもらう（横へ動く必要があれば、その絵） */}
      {locating && state?.locate && (
        <div className="absolute inset-x-0 top-1/2 flex -translate-y-1/2 flex-col items-center gap-3 drop-shadow">
          {state.locate.needMove ? (
            <MovePhone />
          ) : (
            <span className="relative grid size-24 place-items-center">
              <span className="absolute inset-0 animate-pulse rounded-full border-4 border-paper/80" />
              {state.locate.checking && (
                <span className="size-7 animate-spin rounded-full border-2 border-paper border-t-transparent" />
              )}
            </span>
          )}
          <p className="text-center text-base font-semibold">
            {state.locate.needMove
              ? '横へ 30cm ほど動く'
              : state.locate.missed >= 3
                ? '前回と同じ所から映す'
                : '前回撮った所を映す'}
          </p>
        </div>
      )}

      {state?.noCameraImage && (
        <p className="absolute inset-x-6 top-24 rounded-lg bg-black/70 p-3 text-center text-sm text-amber">
          この端末では AR で撮れません（カメラの画像を受け取れません）。
        </p>
      )}

      {/* 対象を向いていない（撮っても緑にならない）ときは、画面の中央に照準を出して「対象に向ける」と伝える */}
      {state && !state.facing && (phase === 'ready' || phase === 'recording') && (
        <span className="absolute left-1/2 top-1/2 size-16 -translate-x-1/2 -translate-y-1/2 animate-pulse rounded-full border-4 border-paper/80" />
      )}

      {/* 次に撮る方向への矢印 */}
      {turn === 'left' && <Chevron className="left-2 top-1/2 -translate-y-1/2" rotate={180} />}
      {turn === 'right' && <Chevron className="right-2 top-1/2 -translate-y-1/2" rotate={0} />}
      {turn === 'up' && <Chevron className="left-1/2 top-28 -translate-x-1/2" rotate={-90} />}

      {/* 下: 録画ボタン */}
      <div className="pad-safe-b absolute inset-x-0 bottom-0 flex flex-col items-center gap-2 pb-6">
        {state && (phase === 'recording' || phase === 'finishing') && (
          <span className="tabular rounded-full bg-black/60 px-3 py-1 text-sm">
            {formatTime(state.elapsed)} ・ {state.picked}
          </span>
        )}
        {phase !== 'placing' && !locating && (
          <button
            type="button"
            aria-label={phase === 'recording' ? '撮影を止める' : '撮り始める'}
            disabled={phase === 'finishing'}
            onClick={phase === 'recording' ? onStop : onRecord}
            className={`pointer-events-auto grid size-[76px] place-items-center rounded-full border-[3px] ${
              loopDone && phase === 'recording' ? 'animate-pulse border-[#34d399]' : 'border-paper'
            } bg-black/20`}
          >
            {phase === 'finishing' ? (
              <span className="size-7 animate-spin rounded-full border-2 border-paper border-t-transparent" />
            ) : (
              <span
                className={`transition-all ${
                  phase === 'recording'
                    ? `size-[28px] rounded-md ${loopDone ? 'bg-[#34d399]' : 'bg-bad'}`
                    : 'size-[58px] rounded-full bg-bad'
                }`}
              />
            )}
          </button>
        )}
      </div>
    </div>
  );
}

/** 端末を左右にゆっくり動かす絵（AR が周りを見つけるには、端末を少し動かす必要がある）。 */
export function MovePhone() {
  return (
    <svg viewBox="0 0 64 40" width="72" height="45" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
      <g className="ar-sway">
        <rect x="24" y="6" width="16" height="28" rx="3" />
        <path d="M30 29h4" />
      </g>
      <path d="M10 20h-6m0 0l3-3m-3 3l3 3M54 20h6m0 0l-3-3m3 3l-3 3" strokeOpacity="0.7" />
    </svg>
  );
}

function RoundButton({ label, onClick, children }: { label: string; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      aria-label={label}
      onClick={onClick}
      className="pointer-events-auto grid size-11 place-items-center rounded-full bg-black/50"
    >
      <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
        {children}
      </svg>
    </button>
  );
}

/**
 * 真上から見たドーム。外の輪 = 横 12、内の輪 = 上 6、真ん中 = 真上。
 * 自分のいる方向がいつも図の下（白い点）に来るよう回す。図の上で次に撮る所が右にあれば、右へ回ればよい。
 */
function DomeMap({ state }: { state: ArState }) {
  const heading = state.az ?? 0;
  const color = (i: number) =>
    state.covered[i] ? '#34d399' : state.target?.tile === i ? '#f5a524' : 'rgba(255,255,255,0.35)';
  const arcs: React.ReactNode[] = [];
  tiles.forEach((t, i) => {
    if (t.band === 'top') return;
    const r = t.band === 'side' ? 26 : 16;
    arcs.push(
      <path
        key={i}
        d={arc(32, 32, r, t.azFrom + 4 - heading, t.azTo - 4 - heading)}
        stroke={color(i)}
        strokeWidth={t.band === 'side' ? 7 : 6}
        fill="none"
        strokeLinecap="round"
      />,
    );
  });
  return (
    <svg viewBox="0 0 64 64" width="84" height="84" className="rounded-full bg-black/45" aria-hidden="true">
      {arcs}
      <circle cx="32" cy="32" r="5" fill={color(tiles.length - 1)} />
      {/* 自分 */}
      <circle cx="32" cy="61" r="3" fill="#ffffff" />
    </svg>
  );
}

/**
 * 円弧の SVG パス。方位角は coverage.ts と同じ（+z が 0°）。真上から見た図として、
 * 0° を図の下（手前）に置き、角度が増えると時計回りに進むように描く。
 */
function arc(cx: number, cy: number, r: number, a0: number, a1: number): string {
  const pt = (a: number) => {
    const rad = (a * Math.PI) / 180;
    return [cx + r * Math.sin(rad), cy + r * Math.cos(rad)] as const;
  };
  const [x0, y0] = pt(a0);
  const [x1, y1] = pt(a1);
  const large = a1 - a0 > 180 ? 1 : 0;
  return `M ${x0.toFixed(2)} ${y0.toFixed(2)} A ${r} ${r} 0 ${large} 0 ${x1.toFixed(2)} ${y1.toFixed(2)}`;
}

/** 大きな矢印（rotate: 0 で右向き）。ゆっくり点滅して目を引く。 */
function Chevron({ className, rotate }: { className: string; rotate: number }) {
  return (
    <svg
      viewBox="0 0 48 48"
      width="64"
      height="64"
      className={`absolute animate-pulse drop-shadow-lg ${className}`}
      style={{ transform: `rotate(${rotate}deg)` }}
      aria-hidden="true"
    >
      <path d="M16 8l16 16-16 16" fill="none" stroke="#f5a524" strokeWidth="7" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function formatTime(sec: number): string {
  const s = Math.floor(sec);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}
