// 「/scans/:id/view」の画面。出来上がった3Dデータを見る。
//
// 画面の構成:
//   上: 戻るボタンと、表示を切り替えるタブ（メッシュ / 3DGS / 点群）
//   中: 3D の表示領域（画面のほとんどを使う）
//   下: 読み込み状況、視点リセット、ダウンロード
import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useLocation, useNavigate, useParams } from 'react-router-dom';
import { fetchSceneInfo, renameScan, requestSplat } from '../api/client';
import type { Scan, SceneInfo, ServerConfig } from '../api/types';
import { arSupported } from '../capture/arSupport';
import { formatBytes } from '../capture/validation';
import { displayTitle } from '../gallery/gallery';
import { useScan } from '../progress/useScan';
import { Button, buttonClass } from '../ui/Button';
import {
  AddPhotoIcon,
  AlertIcon,
  BackIcon,
  CloseIcon,
  CubeIcon,
  DownloadIcon,
  PencilIcon,
  ResetViewIcon,
  SpinnerIcon,
} from '../ui/icons';
import type { ArViewController, ArViewState } from '../viewer/arView';
import { ArViewHud } from '../viewer/ArViewHud';
import { computeViewFrame } from '../viewer/sceneFrame';
import { buildDownloads, buildTabs, defaultTab, type TabKey, type ViewerTab } from '../viewer/tabs';
import type { ViewerHandle, ViewerState } from '../viewer/types';

// ビューアはタブごとに分けて、開いたときに読み込む。メッシュと点群は three.js、3DGS は PlayCanvas を使う。
// まとめると約 2.8MB（圧縮後 708KB）になり、最初に開くメッシュを見るだけでも PlayCanvas まで落としていた。
const MeshViewer = lazy(() => import('../viewer/MeshViewer').then((m) => ({ default: m.MeshViewer })));
const SplatViewer = lazy(() => import('../viewer/SplatViewer').then((m) => ({ default: m.SplatViewer })));
const PointCloudViewer = lazy(() =>
  import('../viewer/PointCloudViewer').then((m) => ({ default: m.PointCloudViewer })),
);

export function ViewPage({ config }: { config: ServerConfig }) {
  const { id = '' } = useParams();
  const { load } = useScan(id);

  if (load.kind === 'ready') return <Viewer scan={load.scan} splatEnabled={config.pipeline.splatEnabled} />;

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

function Viewer({ scan, splatEnabled }: { scan: Scan; splatEnabled: boolean }) {
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

  const ar = useArView();
  // AR で見るのはメッシュだけ（向きと大きさに scene.json を使う）。
  const arTarget = active?.key === 'mesh' && active.url && info ? { url: active.url, info } : null;

  // 生成できなかった出力。隠さずに一覧で示す。
  const missing = tabs.filter((t) => t.url === null);

  // ギャラリーから開いたときは、戻る先もギャラリーにする（それ以外は処理状況の画面）。
  const fromGallery = (useLocation().state as { from?: string } | null)?.from === 'gallery';
  const backTo = fromGallery ? '/gallery' : `/scans/${scan.id}`;

  return (
    <div className="flex h-dvh flex-col">
      <header className="pad-safe-t flex items-center gap-2 border-b border-line bg-panel px-2 pb-2">
        <Link to={backTo} aria-label={fromGallery ? 'ギャラリーに戻る' : '処理状況に戻る'} className="grid size-11 shrink-0 place-items-center">
          <BackIcon />
        </Link>
        <Tabs tabs={tabs} active={activeKey} onSelect={setChosen} />
      </header>

      <div className="relative min-h-0 flex-1 bg-ink">
        {active?.url && frame && (
          // key を付けると、タブを切り替えたときに React が古いビューアを確実に破棄して作り直す。
          // WebGL の資源を持つ部品は、使い回すより作り直す方が安全。
          <div key={active.key} className="absolute inset-0">
            {/* ビューアのプログラムを読み込んでいる間も「読み込んでいます」を出す。 */}
            <Suspense fallback={<Loading />}>
              {active.key === 'mesh' && <MeshViewer ref={viewer} url={active.url} frame={frame} onState={onState} />}
              {active.key === 'splat' && <SplatViewer ref={viewer} url={active.url} frame={frame} onState={onState} />}
              {active.key === 'points' && (
                <PointCloudViewer ref={viewer} url={active.url} frame={frame} info={info ?? null} onState={onState} />
              )}
            </Suspense>
          </div>
        )}

        {/* 読み込み中・エラーは表示領域の中央に重ねる。pointer-events-none で下の3D操作を邪魔しない。 */}
        {active?.url && state.kind === 'loading' && <Loading />}
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
        <TitleRow scan={scan} />
        <Versions scan={scan} />
        {active?.note && <p className="text-xs text-amber">{active.note}</p>}
        {missing.length > 0 && (
          <ul className="space-y-1">
            {missing.map((tab) => (
              <li key={tab.key} className="flex items-start gap-1.5 text-xs text-mute">
                <AlertIcon width={14} height={14} className="mt-0.5 shrink-0 text-amber" />
                <span>
                  <span className="text-paper">{tab.label}なし:</span> {tab.reason}
                  {tab.key === 'splat' && splatEnabled && <MakeSplatButton scan={scan} />}
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
          {ar.ok && arTarget && (
            <Button className="col-span-2" onClick={() => ar.start(arTarget.url, arTarget.info)} disabled={ar.active}>
              <CubeIcon />
              AR で見る
            </Button>
          )}
          {ar.notice && <p className="col-span-2 text-xs text-amber">{ar.notice}</p>}
          {scan.resumable && (
            // 位置合わせ（VPS）をしながら撮り足し、この3Dデータを作り直す。
            <Link to={`/scans/${scan.id}/resume`} className={buttonClass('secondary', 'col-span-2')}>
              <AddPhotoIcon />
              続きを撮る（位置を合わせて撮り足す）
            </Link>
          )}
        </div>
      </footer>

      {/* AR の上に重ねる画面の根。AR の間はブラウザが全画面に広げる。ふだんは空で場所を取らない。 */}
      <div ref={ar.root}>
        {ar.active && (
          <ArViewHud
            state={ar.state}
            onClose={() => ar.ctl.current?.end()}
            onReplace={() => ar.ctl.current?.replace()}
            onRotate={(r) => ar.ctl.current?.rotateBy(r)}
            onZoom={(f) => ar.ctl.current?.zoomBy(f)}
            onResetZoom={() => ar.ctl.current?.resetZoom()}
          />
        )}
      </div>

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

/** 名前と、名前の変更。名前はギャラリーの一覧に出る。 */
function TitleRow({ scan }: { scan: Scan }) {
  // サーバーに保存できた名前。変更のたびにスキャン全体を読み直さず、ここだけ書き換える。
  const [title, setTitle] = useState(scan.title);
  const [error, setError] = useState<string | null>(null);
  const name = displayTitle({ title, createdAt: scan.createdAt });

  const rename = async () => {
    // window.prompt は見た目を選べないが、スマホでも確実に文字入力欄とキーボードが出る。
    const next = window.prompt('名前（80文字まで。空にすると撮影日時を表示します）', title);
    if (next === null) return; // キャンセル
    setError(null);
    try {
      setTitle(await renameScan(scan.id, next));
    } catch (err) {
      setError(err instanceof Error ? err.message : '名前を変更できませんでした。');
    }
  };

  return (
    <div>
      <div className="flex items-center gap-1">
        <p className="min-w-0 flex-1 truncate text-sm font-semibold">{name}</p>
        <button type="button" onClick={() => void rename()} className="-mr-2 flex min-h-11 shrink-0 items-center gap-1 px-2 text-xs text-mute">
          <PencilIcon width={16} height={16} />
          名前を変更
        </button>
      </div>
      {error && <p className="text-xs text-bad">{error}</p>}
    </div>
  );
}

/**
 * 版のつながり。「続きを撮る」で作った版なら前の版へ、新しい版があればそちらへの道を出す。
 * 前の版はギャラリーの一覧から外れる（新しい版が完成すると隠れる）ので、ここから辿れるようにする。
 */
function Versions({ scan }: { scan: Scan }) {
  const newer = scan.continuations.find((c) => c.status === 'succeeded' || c.status === 'partial');
  const running = scan.continuations.find((c) => c.status === 'queued' || c.status === 'running');
  const added = scan.frameCount - scan.baseFrameCount;
  if (!newer && !running && scan.baseFrameCount === 0) return null;
  const link = 'underline underline-offset-4';
  return (
    <p className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-mute">
      {scan.baseFrameCount > 0 && (
        <span>
          前の版の{scan.baseFrameCount}枚に{added}枚を足した版
          {scan.parentId && (
            <>
              {' · '}
              <Link to={`/scans/${scan.parentId}/view`} className={`${link} text-paper`}>
                前の版
              </Link>
            </>
          )}
        </span>
      )}
      {newer && (
        <Link to={`/scans/${newer.id}/view`} className={`${link} text-amber`}>
          新しい版があります
        </Link>
      )}
      {!newer && running && (
        <Link to={`/scans/${running.id}`} className={`${link} text-paper`}>
          続きの版を処理中
        </Link>
      )}
    </p>
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

/** 3D データやビューアを読み込んでいる間の表示。 */
function Loading() {
  return (
    <p className="pointer-events-none absolute inset-0 flex items-center justify-center gap-2 text-sm text-mute">
      <SpinnerIcon />
      読み込んでいます…
    </p>
  );
}

/**
 * 「3DGS を作る」。3DGS は自動では作らない設定があるので、無いときにここからも頼めるようにする。
 * 頼んだら、学習の進み具合が見える処理状況の画面へ移る。
 */
function MakeSplatButton({ scan }: { scan: Scan }) {
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // 位置推定の結果（地図）が残っている完成したスキャンで、まだ頼んでいないときだけ。
  if (!scan.resumable || scan.splatJob || (scan.status !== 'succeeded' && scan.status !== 'partial')) return null;
  const make = async () => {
    if (!window.confirm('3DGS を作ります。CPU では数十分かかり、新しいスキャンがあればそちらを先に処理します。')) return;
    setBusy(true);
    setError(null);
    try {
      await requestSplat(scan.id);
      navigate(`/scans/${scan.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : '受け付けられませんでした。');
      setBusy(false);
    }
  };
  return (
    <>
      {' '}
      <button
        type="button"
        disabled={busy}
        onClick={() => void make()}
        className="text-amber underline underline-offset-4 disabled:text-mute"
      >
        3DGS を作る
      </button>
      {error && <span className="block text-bad">{error}</span>}
    </>
  );
}

/**
 * AR で見る（viewer/arView.ts）。対応端末（Android の Chrome）だけ使える。
 * AR 本体（three.js を含む）は、AR が使える端末でだけ先に読み込んでおく。ボタンを押してから読み込むと、
 * ブラウザが「操作の直後」と認めず AR を始めさせないことがある。
 */
function useArView() {
  const [ok, setOk] = useState(false);
  const [active, setActive] = useState(false);
  const [state, setState] = useState<ArViewState | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const root = useRef<HTMLDivElement>(null);
  const ctl = useRef<ArViewController | null>(null);
  const module = useRef<typeof import('../viewer/arView') | null>(null);

  useEffect(() => {
    let alive = true;
    void arSupported().then((supported) => {
      if (!alive || !supported) return;
      setOk(true);
      void import('../viewer/arView').then((m) => {
        module.current = m;
      });
    });
    return () => {
      alive = false;
      ctl.current?.end();
    };
  }, []);

  const start = (meshUrl: string, info: SceneInfo) => {
    const overlay = root.current;
    if (!overlay || active) return;
    setNotice(null);
    setState(null);
    setActive(true);
    const begin = (m: typeof import('../viewer/arView')) =>
      m.startArView({
        overlay,
        meshUrl,
        info,
        onState: setState,
        onEnded: () => {
          ctl.current = null;
          setActive(false);
          setState(null);
        },
      });
    // 読み込み済みなら、この操作の中ですぐ始める。
    const started = module.current ? begin(module.current) : import('../viewer/arView').then(begin);
    started
      .then((c) => {
        ctl.current = c;
      })
      .catch((err: unknown) => {
        setActive(false);
        setNotice(err instanceof Error ? err.message : 'AR を始められませんでした。');
      });
  };

  return { ok, active, state, notice, root, ctl, start };
}
