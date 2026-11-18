// 「/scans/:id」の画面。処理の進行状況を表示し、終わったらビューアへ案内する。
import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { deleteScan } from '../api/client';
import { isTerminal, type Scan } from '../api/types';
import { ReconstructionProgress } from '../progress/ReconstructionProgress';
import { useNow, useScan } from '../progress/useScan';
import { Button, buttonClass } from '../ui/Button';
import { AddPhotoIcon, AlertIcon, BackIcon, CheckIcon, CubeIcon, GridIcon, SpinnerIcon } from '../ui/icons';

export function ScanPage() {
  const { id = '' } = useParams();
  const { load, refresh } = useScan(id);
  const active = load.kind === 'ready' && !isTerminal(load.scan.status);
  const now = useNow(active);

  return (
    <div className="mx-auto flex min-h-dvh max-w-xl flex-col">
      <header className="pad-safe-t flex items-center justify-between px-4 pb-2">
        <Link to="/" className="-ml-2 flex min-h-11 items-center gap-1 px-2 text-sm">
          <BackIcon />
          撮影画面
        </Link>
        <Link to="/gallery" className="-mr-2 flex min-h-11 items-center gap-1.5 px-2 text-sm">
          <GridIcon />
          ギャラリー
        </Link>
      </header>

      <main className="flex-1 px-4 pb-6">
        {load.kind === 'loading' && (
          <p className="flex items-center gap-2 py-10 text-sm text-mute">
            <SpinnerIcon />
            読み込んでいます…
          </p>
        )}
        {load.kind === 'not-found' && (
          <Message title="スキャンが見つかりません" detail="URL が違うか、保存期限を過ぎて削除された可能性があります。" />
        )}
        {load.kind === 'error' && <Message title="読み込めませんでした" detail={load.message} onRetry={refresh} />}
        {load.kind === 'ready' && (
          <>
            {load.stale && (
              <p className="mb-4 flex items-start gap-2 rounded-lg border border-line bg-panel p-3 text-sm text-amber">
                <AlertIcon className="mt-0.5 shrink-0" />
                <span>通信が不安定です。表示は少し前の状態かもしれません。自動で再接続します。</span>
              </p>
            )}
            {load.scan.baseFrameCount > 0 && <ContinuationNote scan={load.scan} />}
            <ReconstructionProgress scan={load.scan} now={now} />
          </>
        )}
      </main>

      {load.kind === 'ready' && <Actions scan={load.scan} onChanged={refresh} />}
    </div>
  );
}

/** 画面下部の操作。状態によって出すボタンが変わる。 */
function Actions({ scan, onChanged }: { scan: Scan; onChanged: () => void }) {
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const has = (kind: string) => scan.assets.some((a) => a.kind === kind);
  // 3Dとして見られるもの（メッシュか 3DGS）があるか。
  const viewable = has('mesh_glb') || has('gaussian_sog') || has('gaussian_ply');
  // それらは無いが、SfM の点群だけはある（両方の分岐が失敗した場合）。
  const pointsOnly = !viewable && has('sparse_ply');
  const active = !isTerminal(scan.status);
  const gone = scan.status === 'cancelled' || scan.status === 'expired';

  const remove = async (question: string) => {
    // 取り消せない操作なので、必ず確認を挟む。
    if (!window.confirm(question)) return;
    setBusy(true);
    setError(null);
    try {
      const result = await deleteScan(scan.id);
      if (result === 'deleted') {
        navigate('/gallery');
      } else {
        onChanged();
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : '操作に失敗しました。');
    } finally {
      setBusy(false);
    }
  };

  return (
    <footer className="pad-safe-b sticky bottom-0 space-y-2.5 border-t border-line bg-panel px-4 pt-3">
      {error && <p className="text-sm text-bad">{error}</p>}

      {active && (
        <>
          <p className="text-xs leading-relaxed text-mute">
            この画面を閉じても処理は続きます。ギャラリーからいつでも進み具合と結果を確認できます。
          </p>
          <Button variant="danger" className="w-full" disabled={busy} onClick={() => void remove('処理を中止して、写真を削除します。よろしいですか？')}>
            処理を中止する
          </Button>
        </>
      )}

      {!active && (viewable || pointsOnly) && (
        <Link to={`/scans/${scan.id}/view`} className={buttonClass(viewable ? 'primary' : 'secondary', 'w-full')}>
          <CubeIcon />
          {viewable ? '3Dで見る' : 'カメラ位置と点群だけ見る'}
        </Link>
      )}

      {!active && scan.resumable && (
        <Link to={`/scans/${scan.id}/resume`} className={buttonClass('secondary', 'w-full')}>
          <AddPhotoIcon />
          続きを撮る
        </Link>
      )}

      {!active && !viewable && (
        // 続きの版が失敗したときは、前の版から撮り足し直すのが近道。
        <Link
          to={scan.parentId ? `/scans/${scan.parentId}/resume` : '/'}
          className={buttonClass(pointsOnly ? 'secondary' : 'primary', 'w-full')}
        >
          {scan.parentId ? '前の版から撮り直す' : 'もう一度撮る'}
        </Link>
      )}

      {!active && !gone && (
        <div className="flex items-center justify-between gap-3">
          {scan.saved ? (
            <p className="flex items-center gap-1.5 text-xs text-ok">
              <CheckIcon width={16} height={16} />
              ギャラリーに保存済み
            </p>
          ) : (
            <p className="tabular text-xs text-mute">{expiryText(scan.expiresAt)}</p>
          )}
          <button
            type="button"
            disabled={busy}
            onClick={() => void remove(scan.saved ? 'ギャラリーから削除し、写真と3Dデータをサーバーから消します。元に戻せません。よろしいですか？' : '写真と結果をサーバーから削除します。元に戻せません。よろしいですか？')}
            className="min-h-11 px-2 text-sm text-bad underline underline-offset-4"
          >
            {scan.saved ? '削除' : '今すぐ削除'}
          </button>
        </div>
      )}
    </footer>
  );
}

/** 続きの版であることの説明。前の版は書き換えないことも伝える。 */
function ContinuationNote({ scan }: { scan: Scan }) {
  const added = scan.frameCount - scan.baseFrameCount;
  return (
    <p className="mb-4 rounded-lg border border-line bg-panel p-3 text-sm text-mute">
      前の版の{scan.baseFrameCount}枚に、今回の{added}枚を足した版です。
      {'今回の写真を前の版と同じ座標に合わせてから、3Dデータを作り直します。前の版はそのまま残ります。'}
      {scan.parentId && (
        <>
          {' '}
          <Link to={`/scans/${scan.parentId}/view`} className="text-paper underline underline-offset-4">
            前の版を見る
          </Link>
        </>
      )}
    </p>
  );
}

/** 保存期限までの残りを言葉にする。 */
function expiryText(expiresAt: string): string {
  const hours = (Date.parse(expiresAt) - Date.now()) / 3_600_000;
  if (hours <= 0) return 'まもなく自動で削除されます';
  if (hours < 1) return `約${Math.max(1, Math.round(hours * 60))}分後に自動で削除されます`;
  return `約${Math.round(hours)}時間後に自動で削除されます`;
}

function Message({ title, detail, onRetry }: { title: string; detail: string; onRetry?: () => void }) {
  return (
    <div className="space-y-4 py-10">
      <h2 className="text-xl font-semibold">{title}</h2>
      <p className="text-sm text-mute">{detail}</p>
      <div className="flex gap-3">
        {onRetry && <Button onClick={onRetry}>もう一度試す</Button>}
        <Link to="/" className={buttonClass('primary')}>
          撮影画面へ
        </Link>
      </div>
    </div>
  );
}
