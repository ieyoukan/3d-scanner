// AR で見ている間に、カメラ映像の上に重ねる画面（WebXR の DOM overlay）。
//
//   上      閉じる・置き直す
//   真ん中  置く前: 「ゆっくり動かす」→「置く所をタップ」
//           置いた後: 1本指で左右になぞると回る、2本指でつまむと大きさが変わる（画面全体で受ける）
//   下      大きさ（「実物大」または倍率）。押すと元の大きさに戻る
import { useEffect, useRef } from 'react';
import { MovePhone } from '../capture/ArHud';
import type { ArViewState } from './arView';

interface Props {
  state: ArViewState | null;
  onClose: () => void;
  onReplace: () => void;
  onRotate: (radians: number) => void;
  onZoom: (factor: number) => void;
  onResetZoom: () => void;
}

/** 1本指で画面の幅をなぞったときに回る角度（ラジアン）。 */
const radiansPerScreen = 2 * Math.PI;

export function ArViewHud({ state, onClose, onReplace, onRotate, onZoom, onResetZoom }: Props) {
  const root = useRef<HTMLDivElement>(null);
  const phase = state?.phase ?? 'loading';

  // ボタンや操作面を触ったときに、AR の「画面のタップ」（置く）が起きないようにする。
  // 置く前は操作面が無いので、画面のタップがそのまま「置く」になる。
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const block = (e: Event) => {
      if ((e.target as Element | null)?.closest('button, [data-gesture]')) e.preventDefault();
    };
    el.addEventListener('beforexrselect', block);
    return () => el.removeEventListener('beforexrselect', block);
  }, []);

  const gesture = useGesture(onRotate, onZoom);

  const sizeLabel = !state
    ? ''
    : state.realScale
      ? Math.abs(state.zoom - 1) < 0.01
        ? '実物大'
        : `実物の ${formatZoom(state.zoom)} 倍`
      : `目安の大きさ${Math.abs(state.zoom - 1) < 0.01 ? '' : ` × ${formatZoom(state.zoom)}`}`;

  return (
    <div ref={root} className="pointer-events-none fixed inset-0 z-50 select-none text-paper">
      {/* 置いた後は、画面全体で回す・大きさを変える操作を受ける。 */}
      {phase === 'placed' && <div data-gesture className="pointer-events-auto absolute inset-0 touch-none" {...gesture} />}

      <div className="pad-safe-t relative flex items-start justify-between px-3 pt-2">
        <RoundButton label="閉じる" onClick={onClose}>
          <path d="M6 6l12 12M18 6L6 18" />
        </RoundButton>
        {phase === 'placed' ? (
          <RoundButton label="置き直す" onClick={onReplace}>
            <circle cx="12" cy="12" r="7" />
            <circle cx="12" cy="12" r="1.5" fill="currentColor" />
          </RoundButton>
        ) : (
          <span className="size-11" />
        )}
      </div>

      {state?.error ? (
        <p className="absolute inset-x-6 top-1/2 rounded-lg bg-black/70 p-3 text-center text-sm text-amber">{state.error}</p>
      ) : phase === 'loading' ? (
        <span className="absolute left-1/2 top-1/2 size-8 -translate-x-1/2 -translate-y-1/2 animate-spin rounded-full border-2 border-paper border-t-transparent" />
      ) : (
        phase === 'placing' && (
          <div className="absolute inset-x-0 top-1/2 mt-16 flex flex-col items-center gap-2 drop-shadow">
            {!state?.aiming && <MovePhone />}
            <p className="text-center text-base font-semibold">{state?.aiming ? '置く所をタップ' : 'ゆっくり動かす'}</p>
          </div>
        )
      )}

      {phase === 'placed' && state && (
        <div className="pad-safe-b absolute inset-x-0 bottom-0 flex flex-col items-center gap-2 pb-6">
          <button
            type="button"
            onClick={onResetZoom}
            aria-label="元の大きさに戻す"
            className="tabular pointer-events-auto rounded-full bg-black/60 px-4 py-2 text-sm"
          >
            {sizeLabel}
          </button>
          <GestureHint />
        </div>
      )}
    </div>
  );
}

/**
 * 1本指で左右になぞると回し、2本指でつまむと大きさを変える。
 * Pointer Events で指ごとの位置を覚えておき、動くたびに前回との差を渡す。
 */
function useGesture(onRotate: (radians: number) => void, onZoom: (factor: number) => void) {
  const points = useRef(new Map<number, { x: number; y: number }>());
  const spread = () => {
    const [a, b] = [...points.current.values()];
    return a && b ? Math.hypot(a.x - b.x, a.y - b.y) : 0;
  };
  return {
    onPointerDown: (e: React.PointerEvent) => {
      (e.target as Element).setPointerCapture?.(e.pointerId);
      points.current.set(e.pointerId, { x: e.clientX, y: e.clientY });
    },
    onPointerMove: (e: React.PointerEvent) => {
      const prev = points.current.get(e.pointerId);
      if (!prev) return;
      if (points.current.size === 1) {
        onRotate(((e.clientX - prev.x) / window.innerWidth) * radiansPerScreen);
        points.current.set(e.pointerId, { x: e.clientX, y: e.clientY });
      } else if (points.current.size === 2) {
        const before = spread();
        points.current.set(e.pointerId, { x: e.clientX, y: e.clientY });
        const after = spread();
        if (before > 10 && after > 10) onZoom(after / before);
      }
    },
    onPointerUp: (e: React.PointerEvent) => points.current.delete(e.pointerId),
    onPointerCancel: (e: React.PointerEvent) => points.current.delete(e.pointerId),
  };
}

/** 回す・つまむ操作の絵（文字を使わずに伝える）。 */
function GestureHint() {
  return (
    <svg viewBox="0 0 96 28" width="96" height="28" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" className="opacity-70" aria-hidden>
      {/* 左右の矢印（回す） */}
      <path d="M6 14h28M6 14l4-4M6 14l4 4M34 14l-4-4M34 14l-4 4" />
      {/* 外向きの斜めの矢印（大きくする） */}
      <path d="M62 22l8-8M78 6l-8 8M62 22v-5M62 22h5M78 6v5M78 6h-5" />
    </svg>
  );
}

function formatZoom(z: number): string {
  return z >= 10 ? z.toFixed(0) : z.toFixed(1);
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
