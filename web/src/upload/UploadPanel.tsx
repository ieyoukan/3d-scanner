// 送信前の要約と「3Dモデルを生成」ボタン、送信中の進捗を表示する。
//
// 状態遷移:
//
//   idle ──開始──▶ sending ──成功──▶（親が処理画面へ移動）
//                     │
//                     ├──失敗──▶ idle（エラーを表示。写真はそのまま残るので再送できる）
//                     └──中止──▶ idle
import { useEffect, useRef, useState } from 'react';
import type { CreateScanResult, Limits, PipelineInfo } from '../api/types';
import type { Frame } from '../capture/frames';
import { formatBytes, type Check } from '../capture/validation';
import { Button } from '../ui/Button';
import { AlertIcon, InfoIcon } from '../ui/icons';
import { uploadScan, type UploadHandle, type UploadProgress } from './uploadScan';

interface Props {
  frames: readonly Frame[];
  check: Check;
  limits: Limits;
  retentionHours: number;
  /** true なら、完成した3Dデータはギャラリーに保存され、期限で消えない。 */
  keepCompleted: boolean;
  pipeline: PipelineInfo;
  /** サーバーが受け付けたら呼ばれる。 */
  onCreated: (result: CreateScanResult) => void;
  /**
   * 「続きを撮る」のときに指定する。写真はこのスキャンの続きとして送られ、
   * サーバーは元の写真と地図を引き継いだ新しい版を作る。
   */
  resume?: { scanId: string; baseFrameCount: number };
}

export function UploadPanel({ frames, check, limits, retentionHours, keepCompleted, pipeline, onCreated, resume }: Props) {
  const [progress, setProgress] = useState<UploadProgress | null>(null);
  const [error, setError] = useState<string | null>(null);
  const handle = useRef<UploadHandle | null>(null);
  const sending = progress !== null;

  // 画面を離れたら送信を止める（結果を受け取る人が居なくなるため）。
  useEffect(() => () => handle.current?.abort(), []);

  const start = async () => {
    setError(null);
    setProgress({ sentBytes: 0, totalBytes: check.totalBytes });
    const upload = uploadScan(frames, setProgress, resume?.scanId);
    handle.current = upload;
    try {
      onCreated(await upload.done);
    } catch (err) {
      setProgress(null);
      if (err instanceof DOMException && err.name === 'AbortError') return; // 自分で中止した
      setError(err instanceof Error ? err.message : 'アップロードに失敗しました。');
    } finally {
      handle.current = null;
    }
  };

  const percent = progress && progress.totalBytes > 0 ? Math.round((progress.sentBytes / progress.totalBytes) * 100) : 0;

  return (
    <section className="space-y-3">
      {resume ? (
        <p className="tabular text-sm text-mute">
          今回 <span className="text-base font-semibold text-paper">{frames.length}枚</span>
          <span>（あと{limits.maxImages}枚まで）</span>
          <span className="mx-1.5">・</span>
          前回の{resume.baseFrameCount}枚と合わせて{resume.baseFrameCount + frames.length}枚
          <span className="mx-1.5">・</span>
          {formatBytes(check.totalBytes)}
        </p>
      ) : (
        <p className="tabular text-sm text-mute">
          <span className="text-base font-semibold text-paper">{frames.length}枚</span>
          <span>（{limits.minImages}〜{limits.maxImages}枚）</span>
          <span className="mx-1.5">・</span>
          合計 {formatBytes(check.totalBytes)} / {formatBytes(limits.maxUploadBytes)}
        </p>
      )}

      {check.issues.length > 0 && (
        <ul className="space-y-1.5">
          {check.issues.map((issue) => (
            <li
              key={issue.text}
              className={`flex items-start gap-2 text-sm ${issue.level === 'error' ? 'text-bad' : 'text-amber'}`}
            >
              <AlertIcon className="mt-0.5 shrink-0" />
              <span>{issue.text}</span>
            </li>
          ))}
        </ul>
      )}

      {error && (
        <p role="alert" className="flex items-start gap-2 rounded-lg border border-bad/50 bg-panel p-3 text-sm text-bad">
          <AlertIcon className="mt-0.5 shrink-0" />
          <span>{error}</span>
        </p>
      )}

      {sending ? (
        <div className="space-y-2">
          <div className="flex items-baseline justify-between text-sm">
            <span>写真を送信しています</span>
            <span className="tabular text-mute">
              {formatBytes(progress.sentBytes)} / {formatBytes(progress.totalBytes)}（{percent}%）
            </span>
          </div>
          {/* 送信済みバイト数に基づく本物の進捗。 */}
          <div
            role="progressbar"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={percent}
            className="h-1.5 overflow-hidden rounded-full bg-raised"
          >
            <div className="h-full bg-amber transition-[width] duration-200" style={{ width: `${percent}%` }} />
          </div>
          <p className="text-xs text-mute">送信が終わるまで、この画面を開いたままにしてください。</p>
          <Button className="w-full" onClick={() => handle.current?.abort()}>
            送信を中止
          </Button>
        </div>
      ) : (
        <Button variant="primary" className="w-full" disabled={!check.canStart} onClick={() => void start()}>
          {resume ? '写真を追加して作り直す' : '3Dモデルを生成'}
        </Button>
      )}

      <p className="flex items-start gap-2 text-xs leading-relaxed text-mute">
        <InfoIcon width={16} height={16} className="mt-0.5 shrink-0" />
        <span>
          {resume &&
            '前回の写真と位置合わせの結果はそのまま使い、今回の写真を同じ座標に合わせて足します。作り直した3Dデータは新しい版として保存され、前の版も残ります。'}
          {describePipeline(pipeline)}
          {keepCompleted
            ? `完成した3Dデータはギャラリーに保存されます。失敗したものは${formatHours(retentionHours)}後に自動で削除されます。`
            : `写真と結果は${formatHours(retentionHours)}後に自動で削除されます。`}
        </span>
      </p>
    </section>
  );
}

/** このサーバーで何が作られるかを、送る前に正直に伝える。 */
function describePipeline(p: PipelineInfo): string {
  const parts: string[] = [];
  if (p.meshEnabled) {
    parts.push(p.meshMode === 'dense' ? 'メッシュ（密な復元）' : 'メッシュ（簡易版。このサーバーは GPU が無いため粗くなります）');
  }
  if (p.splatEnabled && !p.splatDeferred) {
    parts.push(p.splatDevice === 'cpu' ? '3D Gaussian Splatting（CPU で学習するため時間がかかります）' : '3D Gaussian Splatting');
  }
  if (parts.length === 0 && !p.splatEnabled) return 'カメラ位置と点群だけを求めます。';
  const later = p.splatEnabled && p.splatDeferred ? '3D Gaussian Splatting は、そのあと手が空いたときに作ります。' : '';
  if (parts.length === 0) return later;
  return `${parts.join('と')}を作ります。${later}`;
}

function formatHours(hours: number): string {
  if (hours >= 48) return `${Math.round(hours / 24)}日`;
  return `${Math.round(hours)}時間`;
}
