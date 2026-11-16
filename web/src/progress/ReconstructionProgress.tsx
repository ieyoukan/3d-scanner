// 復元の進行状況を表示する。
//
// 表示するのはサーバーから取得した事実だけ:
//   - 各ステップの状態（待ち / 実行中 / 成功 / 失敗 / 実行せず）
//   - サーバーが返した説明・失敗理由
//   - 実際の開始・終了時刻から計算した所要時間
// 「全体の何%」のような、根拠の無い数字は出さない。
import type { Scan } from '../api/types';
import { AlertIcon, CheckIcon, CloseIcon, DashIcon, SpinnerIcon } from '../ui/icons';
import { LogViewer } from './LogViewer';
import { buildStages, formatDuration, headline, stepDuration, type StageStatus, type StepView } from './stages';

interface Props {
  scan: Scan;
  /** 現在時刻（ミリ秒）。実行中のステップの経過時間を出すために使う。 */
  now: number;
}

const toneClass = {
  neutral: 'text-paper',
  active: 'text-amber',
  good: 'text-ok',
  warn: 'text-amber',
  bad: 'text-bad',
} as const;

export function ReconstructionProgress({ scan, now }: Props) {
  const head = headline(scan);
  const stages = buildStages(scan);

  // 全体の経過時間: 開始から「終了時刻」または「いま」まで。
  const started = scan.startedAt ? Date.parse(scan.startedAt) : null;
  const ended = scan.finishedAt ? Date.parse(scan.finishedAt) : now;
  const elapsed = started !== null ? formatDuration(ended - started) : null;

  return (
    <div className="space-y-5">
      {/* aria-live: 状態が変わったら、読み上げソフトがこの部分を読み直す。 */}
      <div aria-live="polite">
        <h2 className={`text-xl font-semibold ${toneClass[head.tone]}`}>{head.title}</h2>
        <p className="mt-1 text-sm text-mute">{head.detail}</p>
        <p className="tabular mt-2 text-xs text-mute">
          写真 {scan.frameCount}枚
          {elapsed && <span>・経過 {elapsed}</span>}
        </p>
      </div>

      <ol className="space-y-2.5">
        {stages.map((stage, i) => (
          <li key={stage.key} className="rounded-xl border border-line bg-panel p-3.5">
            <div className="flex items-center gap-3">
              <StatusMark status={stage.status} />
              <p className="font-semibold">
                <span className="tabular mr-2 text-mute">{i + 1}</span>
                {stage.title}
              </p>
              <span className="ml-auto text-xs text-mute">{stageLabel[stage.status]}</span>
            </div>
            {stage.steps.length > 0 && (
              <ul className="mt-3 space-y-3 border-t border-line pt-3">
                {stage.steps.map((step) => (
                  <StepRow key={step.name} scanId={scan.id} step={step} now={now} />
                ))}
              </ul>
            )}
          </li>
        ))}
      </ol>
    </div>
  );
}

const stageLabel: Record<StageStatus, string> = {
  pending: '待ち',
  running: '実行中',
  succeeded: '成功',
  failed: '失敗',
  skipped: '実行せず',
  warning: '一部失敗',
};

function StepRow({ scanId, step, now }: { scanId: string; step: StepView; now: number }) {
  const duration = stepDuration(step, now);
  return (
    <li className="grid grid-cols-[1.25rem_1fr] gap-x-2.5">
      <div className="pt-0.5">
        <StatusMark status={step.status} small />
      </div>
      <div className="min-w-0">
        <p className="flex flex-wrap items-baseline gap-x-2 text-sm">
          <span className={step.status === 'pending' ? 'text-mute' : ''}>{step.label}</span>
          {duration !== null && step.status !== 'skipped' && (
            <span className="tabular text-xs text-mute">{formatDuration(duration)}</span>
          )}
        </p>
        <p className="mt-0.5 text-xs leading-relaxed text-mute">{step.explanation}</p>
        {/* 実測の進み具合。ツール自身が出力した値がある場合にだけ表示する。 */}
        {step.status === 'running' && step.progress && step.progress.total > 0 && (
          <div className="mt-2">
            <div
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={step.progress.total}
              aria-valuenow={step.progress.current}
              className="h-1.5 overflow-hidden rounded-full bg-raised"
            >
              <div
                className="h-full bg-amber transition-[width] duration-500"
                style={{ width: `${(step.progress.current / step.progress.total) * 100}%` }}
              />
            </div>
            <p className="tabular mt-1 text-xs text-mute">
              {step.progress.current.toLocaleString()} / {step.progress.total.toLocaleString()} {step.progress.unit}
            </p>
          </div>
        )}
        {/* サーバーが返した補足（成功時の数値や、実行しなかった理由）。 */}
        {step.message && <p className="mt-1.5 text-sm leading-relaxed">{step.message}</p>}
        {step.error && (
          <p className="mt-1.5 flex items-start gap-1.5 text-sm leading-relaxed text-bad">
            <AlertIcon width={16} height={16} className="mt-0.5 shrink-0" />
            <span>{step.error}</span>
          </p>
        )}
        {step.hasLog && <LogViewer scanId={scanId} step={step.name} live={step.status === 'running'} />}
      </div>
    </li>
  );
}

/**
 * 状態を示す印。色だけでなく形（✓ × − 回転）でも区別できるようにしている。
 * 色の見分けにくい人や、屋外の明るい場所でも状態が分かる。
 */
function StatusMark({ status, small = false }: { status: StageStatus; small?: boolean }) {
  const size = small ? 16 : 20;
  const box = `grid shrink-0 place-items-center rounded-full ${small ? 'size-5' : 'size-7'}`;
  switch (status) {
    case 'running':
      return (
        <span className={`${box} text-amber`}>
          <SpinnerIcon width={size} height={size} />
        </span>
      );
    case 'succeeded':
      return (
        <span className={`${box} bg-ok/15 text-ok`}>
          <CheckIcon width={size - 4} height={size - 4} />
        </span>
      );
    case 'failed':
      return (
        <span className={`${box} bg-bad/15 text-bad`}>
          <CloseIcon width={size - 4} height={size - 4} />
        </span>
      );
    case 'warning':
      return (
        <span className={`${box} bg-amber/15 text-amber`}>
          <AlertIcon width={size - 4} height={size - 4} />
        </span>
      );
    case 'skipped':
      return (
        <span className={`${box} text-mute`}>
          <DashIcon width={size - 4} height={size - 4} />
        </span>
      );
    default:
      return <span className={`${box} border border-line`} />;
  }
}
