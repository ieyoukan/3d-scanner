// 「/scans/:id/view」の画面。出来上がった3Dデータを見る。
//
// 画面の構成:
//   上: 戻るボタンと、表示を切り替えるタブ（メッシュ / 3DGS / 点群）
//   中: 3D の表示領域（画面のほとんどを使う）
//   下: 読み込み状況、視点リセット、ダウンロード
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { fetchSceneInfo } from '../api/client';
import type { Scan, SceneInfo } from '../api/types';
import { formatBytes } from '../capture/validation';
import { useScan } from '../progress/useScan';
import { Button, buttonClass } from '../ui/Button';
import { AlertIcon, BackIcon, CloseIcon, DownloadIcon, ResetViewIcon, SpinnerIcon } from '../ui/icons';
import { MeshViewer } from '../viewer/MeshViewer';
import { PointCloudViewer } from '../viewer/PointCloudViewer';
import { computeViewFrame } from '../viewer/sceneFrame';
import { SplatViewer } from '../viewer/SplatViewer';
import { buildDownloads, buildTabs, defaultTab, type TabKey, type ViewerTab } from '../viewer/tabs';
import type { ViewerHandle, ViewerState } from '../viewer/types';

export function ViewPage({ onUnauthorized }: { onUnauthorized: () => void }) {
  const { id = '' } = useParams();
  const { load } = useScan(id);

  const unauthorized = load.kind === 'unauthorized';
  useEffect(() => {
    if (unauthorized) onUnauthorized();
  }, [unauthorized, onUnauthorized]);

  if (load.kind === 'ready') return <Viewer scan={load.scan} />;

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-4 px-6 text-center text-sm text-mute">
      {load.kind === 'loading' && <SpinnerIcon width={24} height={24} />}
      {load.kind === 'not-found' && <p>スキャンが見つかりません。保存期限を過ぎて削除された可能性があります。</p>}
      {load.kind === 'error' && <p>{load.message}</p>}
      {load.kind !== 'loading' && (
        <Link to="/" className={buttonClass('primary')}>
          撮影画面へ
        </Link>
      )}
    </main>
  );
}

function Viewer({ scan }: { scan: Scan }) {
  const tabs = useMemo(() => buildTabs(scan), [scan]);
  const downloads = useMemo(() => buildDownloads(scan), [scan]);
  // 利用者が選んだタブ。null の間は「使えるものの先頭」を表示する。
  const [chosen, setChosen] = useState<TabKey | null>(null);
  const activeKey = chosen ?? defaultTab(tabs);
  const active = tabs.find((t) => t.key === activeKey) ?? null;

  const [state, setState] = useState<ViewerState>({ kind: 'loading' });
  const [showDownloads, setShowDownloads] = useState(false);
  const viewer = useRef<ViewerHandle>(null);

  // 向きと初期視点の手掛かり（scene.json）を読む。
  // undefined = まだ読んでいる / null = 無い・読めなかった（仮の向きで表示する）
  const sceneUrl = scan.assets.find((a) => a.kind === 'scene_json')?.url;
  const [info, setInfo] = useState<SceneInfo | null | undefined>(sceneUrl ? undefined : null);
  useEffect(() => {
    if (!sceneUrl) return;
    const controller = new AbortController();
    fetchSceneInfo(sceneUrl, controller.signal)
      .then(setInfo)
      .catch(() => {
        if (!controller.signal.aborted) setInfo(null);
      });
    return () => controller.abort();
  }, [sceneUrl]);

  // frame は info から計算できる値。useMemo で「info が変わったときだけ作り直す」ようにする。
  // 毎回新しいオブジェクトを作ると、ビューアが「設定が変わった」と判断して読み込み直してしまう。
  const frame = useMemo(() => (info === undefined ? null : computeViewFrame(info)), [info]);

  // ビューアに渡す関数は useCallback で同じものを保つ（理由は frame と同じ）。
  const onState = useCallback((next: ViewerState) => setState(next), []);

  // 生成できなかった出力。隠さずに一覧で示す。
  const missing = tabs.filter((t) => t.url === null);

  return (
    <div className="flex h-dvh flex-col">
      <header className="pad-safe-t flex items-center gap-2 border-b border-line bg-panel px-2 pb-2">
        <Link to={`/scans/${scan.id}`} aria-label="処理状況に戻る" className="grid size-11 shrink-0 place-items-center">
          <BackIcon />
        </Link>
        <Tabs tabs={tabs} active={activeKey} onSelect={setChosen} />
      </header>

      <div className="relative min-h-0 flex-1 bg-ink">
        {active?.url && frame && (
          // key を付けると、タブを切り替えたときに React が古いビューアを確実に破棄して作り直す。
          // WebGL の資源を持つ部品は、使い回すより作り直す方が安全。
          <div key={active.key} className="absolute inset-0">
            {active.key === 'mesh' && <MeshViewer ref={viewer} url={active.url} frame={frame} onState={onState} />}
            {active.key === 'splat' && <SplatViewer ref={viewer} url={active.url} frame={frame} onState={onState} />}
            {active.key === 'points' && (
              <PointCloudViewer ref={viewer} url={active.url} frame={frame} info={info ?? null} onState={onState} />
            )}
          </div>
        )}

        {/* 読み込み中・エラーは表示領域の中央に重ねる。pointer-events-none で下の3D操作を邪魔しない。 */}
        {active?.url && state.kind === 'loading' && (
          <p className="pointer-events-none absolute inset-0 flex items-center justify-center gap-2 text-sm text-mute">
            <SpinnerIcon />
            読み込んでいます…
          </p>
        )}
        {active?.url && state.kind === 'error' && (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 px-6 text-center text-sm">
            <AlertIcon width={28} height={28} className="text-bad" />
            <p>{state.message}</p>
          </div>
        )}
        {!active?.url && (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 px-6 text-center text-sm">
            <AlertIcon width={28} height={28} className="text-amber" />
            <p className="max-w-sm">{active?.reason ?? '表示できる3Dデータがありません。'}</p>
          </div>
        )}
      </div>

      <footer className="pad-safe-b space-y-2 border-t border-line bg-panel px-4 pt-2.5">
        {active?.note && <p className="text-xs text-amber">{active.note}</p>}
        {missing.length > 0 && (
          <ul className="space-y-1">
            {missing.map((tab) => (
              <li key={tab.key} className="flex items-start gap-1.5 text-xs text-mute">
                <AlertIcon width={14} height={14} className="mt-0.5 shrink-0 text-amber" />
                <span>
                  <span className="text-paper">{tab.label}なし:</span> {tab.reason}
                </span>
              </li>
            ))}
          </ul>
        )}
        {/* 要約（点や面の数）は1行使って全部見せる。ボタンと同じ行に置くと、狭い画面で途中が省略される。 */}
        <p className="tabular flex min-h-5 flex-wrap items-baseline justify-between gap-x-3 text-xs text-mute">
          <span className="text-paper">{state.kind === 'ready' ? state.summary : ' '}</span>
          <span>ドラッグで回転・2本指で移動・ピンチで拡大</span>
        </p>
        <div className="grid grid-cols-2 gap-2">
          <Button onClick={() => viewer.current?.resetView()} disabled={state.kind !== 'ready'}>
            <ResetViewIcon />
            視点を戻す
          </Button>
          <Button onClick={() => setShowDownloads(true)} disabled={downloads.length === 0}>
            <DownloadIcon />
            保存
          </Button>
        </div>
      </footer>

      {showDownloads && (
        <div role="dialog" aria-modal="true" aria-label="ファイルを保存" className="fixed inset-0 z-20 flex items-end bg-black/60 sm:items-center sm:justify-center">
          <div className="pad-safe-b w-full space-y-3 rounded-t-2xl border border-line bg-panel px-4 pt-4 sm:max-w-sm sm:rounded-2xl sm:pb-4">
            <div className="flex items-center justify-between">
              <h2 className="font-semibold">ファイルを保存</h2>
              <button type="button" onClick={() => setShowDownloads(false)} aria-label="閉じる" className="grid size-11 place-items-center">
                <CloseIcon />
              </button>
            </div>
            <ul className="space-y-2">
              {downloads.map((d) => (
                <li key={d.kind}>
                  {/* download 属性: リンク先を開かずにファイルとして保存する。 */}
                  <a
                    href={d.href}
                    download={d.fileName}
                    className="flex min-h-11 w-full items-center justify-between gap-3 rounded-lg border border-line bg-raised px-4 text-[15px] hover:border-mute active:bg-line"
                  >
                    <span className="flex items-center gap-2">
                      <DownloadIcon className="text-mute" />
                      {d.label}
                    </span>
                    <span className="tabular text-xs text-mute">{formatBytes(d.sizeBytes)}</span>
                  </a>
                </li>
              ))}
            </ul>
            <p className="text-xs leading-relaxed text-mute">
              ファイルの座標は COLMAP が決めた座標系のままです（画面では見やすい向きに回して表示しています）。
            </p>
          </div>
        </div>
      )}
    </div>
  );
}

/** タブ。使えないタブは押せない状態で残す。 */
function Tabs({ tabs, active, onSelect }: { tabs: ViewerTab[]; active: TabKey | null; onSelect: (key: TabKey) => void }) {
  return (
    <div role="tablist" aria-label="表示する3Dデータ" className="grid flex-1 grid-cols-3 gap-1 rounded-lg bg-ink p-1">
      {tabs.map((tab) => {
        const selected = tab.key === active;
        const available = tab.url !== null;
        return (
          <button
            key={tab.key}
            type="button"
            role="tab"
            aria-selected={selected}
            disabled={!available}
            title={available ? undefined : tab.reason}
            onClick={() => onSelect(tab.key)}
            className={`min-h-9 rounded-md text-sm transition-colors ${
              selected ? 'bg-raised font-semibold text-paper' : available ? 'text-mute hover:text-paper' : 'text-mute/50 line-through'
            }`}
          >
            {tab.label}
          </button>
        );
      })}
    </div>
  );
}
