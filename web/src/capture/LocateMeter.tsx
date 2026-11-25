// 前回の3Dデータを実物に重ねるまでのメーター（「AR で見る」と「続きを撮る」で共通）。arLocator.ts の段階を絵で見せる。
//
//   輪       いまの段階の進み具合（動いた距離・経過秒数・横へ動いた距離。どれも本当に測った量）
//   真ん中   段階の絵（端末を動かす / 照合の経過秒数 / 横へ歩く）。位置を見失ったら「！」
//   下の点   全体のどこまで来たか（覚える → 照合 →（横へ → 照合））
//   一言     やること（できるだけ短く）
import type { LocateProgress } from './arLocator';
import { MovePhone, WalkSideways } from './arIcons';

const size = 120;
const stroke = 8;
const radius = (size - stroke) / 2;
const circumference = 2 * Math.PI * radius;

export function LocateMeter({ progress }: { progress: LocateProgress }) {
  const { stage, amount, lost, checkingSeconds, expectedSeconds, found, missed, needFixes } = progress;
  // 大きさが分からないスキャンは、照合が 2 回要る（間に「横へ」が入る）。
  const steps = found + needFixes >= 2 ? ['warmup', 'checking', 'move', 'checking2'] : ['warmup', 'checking'];
  const current = stage === 'warmup' ? 0 : stage === 'checking' ? (found >= 1 ? 3 : 1) : stage === 'move' ? 2 : steps.length;
  const color = lost ? '#f5a524' : stage === 'aligned' ? '#34d399' : stage === 'checking' ? '#ffffff' : '#f5a524';
  const label = lost
    ? 'ゆっくり動かす'
    : stage === 'warmup'
      ? '周りを写しながら動かす'
      : stage === 'move'
        ? '横へ 30cm ほど動く'
        : stage === 'checking'
          ? missed >= 2
            ? '前回撮った所を映す'
            : '照合中'
          : '重なりました';
  return (
    <div className="flex flex-col items-center gap-2 drop-shadow" role="status" aria-label={label}>
      <div className="relative grid place-items-center" style={{ width: size, height: size }}>
        <svg viewBox={`0 0 ${size} ${size}`} width={size} height={size} className="absolute inset-0 -rotate-90" aria-hidden>
          <circle cx={size / 2} cy={size / 2} r={radius} fill="rgba(0,0,0,0.45)" stroke="rgba(255,255,255,0.25)" strokeWidth={stroke} />
          <circle
            cx={size / 2}
            cy={size / 2}
            r={radius}
            fill="none"
            stroke={color}
            strokeWidth={stroke}
            strokeLinecap="round"
            strokeDasharray={circumference}
            strokeDashoffset={circumference * (1 - (lost ? 0 : amount))}
            style={{ transition: 'stroke-dashoffset 0.3s linear' }}
          />
        </svg>
        <div className="relative text-paper">
          {lost ? (
            <span className="text-4xl font-bold text-amber">!</span>
          ) : stage === 'checking' ? (
            <span className="tabular flex flex-col items-center leading-none">
              <span className="text-3xl font-semibold">{Math.floor(checkingSeconds)}</span>
              <span className="mt-1 text-xs opacity-80">/ {expectedSeconds} 秒</span>
            </span>
          ) : stage === 'move' ? (
            <WalkSideways />
          ) : stage === 'warmup' ? (
            <MovePhone />
          ) : null}
        </div>
      </div>
      <div className="flex gap-1.5" aria-hidden>
        {steps.map((s, i) => (
          <span
            key={s}
            className={`size-2 rounded-full ${i < current ? 'bg-[#34d399]' : i === current ? 'bg-paper' : 'bg-paper/30'}`}
          />
        ))}
      </div>
      <p className="rounded-full bg-black/50 px-3 py-1 text-center text-sm font-semibold text-paper">{label}</p>
    </div>
  );
}
