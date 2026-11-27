// AR で見ている間に、カメラ映像の上に重ねる画面（WebXR の DOM overlay）。
//
//   上      閉じる。重ねた後は右に「透かす」
//   真ん中  照合中: 経過秒数と目安の輪（メーター）。見つからなかったとき: 理由とやり直し方
//   下      大きな「重ねる」ボタン（重ねた後は「重ね直す」）。重ねた後は一致した特徴点の数
import { useEffect, useRef } from 'react';
import type { ArViewState } from './arView';

interface Props {
  state: ArViewState | null;
  onClose: () => void;
  onAlign: () => void;
  onSeeThrough: () => void;
}

export function ArViewHud({ state, onClose, onAlign, onSeeThrough }: Props) {
  const root = useRef<HTMLDivElement>(null);
  const phase = state?.phase ?? 'loading';

  // ボタンを押したときに、AR の「画面のタップ」が起きないようにする。
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const block = (e: Event) => {
      if ((e.target as Element | null)?.closest('button')) e.preventDefault();
    };
    el.addEventListener('beforexrselect', block);
    return () => el.removeEventListener('beforexrselect', block);
  }, []);

  return (
    <div ref={root} className="pointer-events-none fixed inset-0 z-50 select-none text-paper">
      <div className="pad-safe-t relative flex items-start justify-between px-3 pt-2">
        <RoundButton label="閉じる" onClick={onClose}>
          <path d="M6 6l12 12M18 6L6 18" />
        </RoundButton>
        {phase === 'overlay' ? (
          <RoundButton label={state?.seeThrough ? '透かすのをやめる' : '透かして実物と見比べる'} onClick={onSeeThrough}>
            <rect x="4" y="4" width="11" height="11" rx="1.5" />
            <rect x="9" y="9" width="11" height="11" rx="1.5" fill={state?.seeThrough ? 'currentColor' : 'none'} fillOpacity="0.35" />
          </RoundButton>
        ) : (
          <span className="size-11" />
        )}
      </div>

      <div className="absolute inset-x-0 top-1/2 flex -translate-y-1/2 flex-col items-center gap-3 px-6 drop-shadow">
        {state?.error ? (
          <p className="rounded-lg bg-black/70 p-3 text-center text-sm text-amber">{state.error}</p>
        ) : phase === 'loading' ? (
          <span className="size-8 animate-spin rounded-full border-2 border-paper border-t-transparent" />
        ) : phase === 'checking' && state ? (
          <CheckMeter seconds={state.checkingSeconds} expected={state.expectedSeconds} />
        ) : phase === 'failed' && state?.failure ? (
          <p className="max-w-xs rounded-lg bg-black/70 p-3 text-center text-sm">{state.failure}</p>
        ) : phase === 'ready' ? (
          <p className="rounded-full bg-black/50 px-3 py-1 text-center text-sm font-semibold">撮ったものに向けて押す</p>
        ) : null}
      </div>

      <div className="pad-safe-b absolute inset-x-0 bottom-0 flex flex-col items-center gap-2 pb-8">
        {phase === 'overlay' && state?.matches != null && (
          <span className="tabular rounded-full bg-black/60 px-3 py-1 text-xs">特徴点 {state.matches} 個で一致</span>
        )}
        {phase !== 'loading' && !state?.error && (
          <button
            type="button"
            onClick={onAlign}
            disabled={phase === 'checking'}
            className="pointer-events-auto grid size-[76px] place-items-center rounded-full border-[3px] border-paper bg-black/30 disabled:opacity-40"
            aria-label={phase === 'overlay' ? '重ね直す' : '重ねる'}
          >
            <svg viewBox="0 0 24 24" width="30" height="30" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
              <rect x="3" y="5" width="11" height="11" rx="1.5" />
              <rect x="10" y="8" width="11" height="11" rx="1.5" />
            </svg>
          </button>
        )}
        {phase !== 'loading' && !state?.error && (
          <span className="text-sm font-semibold drop-shadow">{phase === 'overlay' ? '重ね直す' : '重ねる'}</span>
        )}
      </div>
    </div>
  );
}

/** 照合中のメーター: 経過秒数を輪で見せる（目安は前回かかった秒数。超えたら輪はいっぱいのまま数字だけ進む）。 */
function CheckMeter({ seconds, expected }: { seconds: number; expected: number }) {
  const size = 120;
  const stroke = 8;
  const r = (size - stroke) / 2;
  const len = 2 * Math.PI * r;
  const amount = Math.min(1, seconds / Math.max(1, expected));
  return (
    <div className="flex flex-col items-center gap-2" role="status" aria-label="照合中">
      <div className="relative grid place-items-center" style={{ width: size, height: size }}>
        <svg viewBox={`0 0 ${size} ${size}`} width={size} height={size} className="absolute inset-0 -rotate-90" aria-hidden>
          <circle cx={size / 2} cy={size / 2} r={r} fill="rgba(0,0,0,0.45)" stroke="rgba(255,255,255,0.25)" strokeWidth={stroke} />
          <circle
            cx={size / 2}
            cy={size / 2}
            r={r}
            fill="none"
            stroke="#ffffff"
            strokeWidth={stroke}
            strokeLinecap="round"
            strokeDasharray={len}
            strokeDashoffset={len * (1 - amount)}
            style={{ transition: 'stroke-dashoffset 0.3s linear' }}
          />
        </svg>
        <span className="tabular relative flex flex-col items-center leading-none">
          <span className="text-3xl font-semibold">{Math.floor(seconds)}</span>
          <span className="mt-1 text-xs opacity-80">/ {expected} 秒</span>
        </span>
      </div>
      <p className="rounded-full bg-black/50 px-3 py-1 text-sm font-semibold">照合中・そのまま待つ</p>
    </div>
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
