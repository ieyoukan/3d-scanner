// 「/scans/:id/resume」の画面。保存済みの3Dデータに写真を撮り足して作り直す（続きを撮る）。
//
// 流れ:
//   1. 現地で1枚撮る → サーバーがその写真を前回の3Dデータ（地図）に位置合わせする（VPS）。
//      地図に「前回の撮影位置」と「いまの位置と向き」が出るので、同じ場所に戻れたか、
//      前回撮っていない側がどちらかが分かる。
//   2. 撮り足す。「最新の写真で確かめる」で、いまどこに居るかを何度でも確かめ直せる。
//   3. 送る → サーバーは前回の写真と地図を引き継いだ新しい版を作り、今回の写真を同じ座標に合わせて
//      足してから、メッシュと 3DGS を作り直す。前の版は書き換えないので、そのまま残る。
//
// 位置合わせは送る前の確認のためのもので、必須ではない。確かめずに送っても、サーバーは同じ方法で
// 今回の写真を地図に合わせる（合わせられた枚数は処理状況の画面に出る）。
import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { fetchSceneInfo } from '../api/client';
import type { Limits, Scan, SceneInfo, ServerConfig } from '../api/types';
import { CaptureView } from '../capture/CaptureView';
import { FrameGallery } from '../capture/FrameGallery';
import { findBlurry } from '../capture/sharpness';
import { useFrames } from '../capture/useFrames';
import { checkFrames } from '../capture/validation';
import { displayTitle } from '../gallery/gallery';
import { useScan } from '../progress/useScan';
import { ResumeMap } from '../resume/ResumeMap';
import { useLocalize, type Fix, type LocalizeState } from '../resume/useLocalize';
import { Button, buttonClass } from '../ui/Button';
import { AlertIcon, BackIcon, LocateIcon, SpinnerIcon } from '../ui/icons';
import { UploadPanel } from '../upload/UploadPanel';

export function ResumePage({ config }: { config: ServerConfig }) {
  const { id = '' } = useParams();
  const { load } = useScan(id);

  if (load.kind === 'ready' && load.scan.resumable) return <Resume scan={load.scan} config={config} />;

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-4 px-6 text-center text-sm text-mute">
      {load.kind === 'loading' && <SpinnerIcon width={24} height={24} />}
      {load.kind === 'not-found' && <p>スキャンが見つかりません。削除された可能性があります。</p>}
      {load.kind === 'error' && <p>{load.message}</p>}
      {load.kind === 'ready' && (
        <p className="max-w-sm">
          このスキャンには続きを撮れません。完成していて、位置合わせ用の地図（カメラの位置と特徴点）が残っているものだけが対象です。
        </p>
      )}
      {load.kind !== 'loading' && (
        <Link to="/gallery" className={buttonClass('primary')}>
          ギャラリーへ
        </Link>
      )}
    </main>
  );
}

type Phase = 'capture' | 'review';

function Resume({ scan, config }: { scan: Scan; config: ServerConfig }) {
  const navigate = useNavigate();
  // 引き継ぐ写真の枚数。サーバーは元の版の写真を全部引き継ぐ。
  const base = scan.frameCount;
  // 処理には全部の写真を使うので、上限は引き継ぐ写真と合わせた枚数で決まる。今回は1枚からでよい。
  const room = config.limits.maxImages - base;
  const limits = useMemo<Limits>(
    () => ({ ...config.limits, minImages: 1, maxImages: Math.max(room, 0) }),
    [config.limits, room],
  );

  const { frames, add, remove, clear } = useFrames(limits.maxImages);
  const [phase, setPhase] = useState<Phase>('capture');
  // 送信中は撮影を止める（自動撮影が、送信中の一覧に写真を足さないように）。
  const [sending, setSending] = useState(false);
  const info = useSceneInfo(scan);
  const { state, fixes, check: locate, forget, reset } = useLocalize(scan.id);

  // 1枚目を撮ったら、自動で位置を確かめる（いちばん知りたいのは「同じ場所に戻れたか」なので）。
  const first = frames[0];
  useEffect(() => {
    if (first && state.kind === 'idle') void locate(first);
  }, [first, state.kind, locate]);

  // 「確かめる」は最新の写真で行う。同じ写真を確認中なら押せない。
  const latest = frames.at(-1);
  const checkingLatest = state.kind === 'checking' && state.frameId === latest?.id;
  const checkLatest = latest && !checkingLatest ? () => void locate(latest) : undefined;

  const removeFrame = useCallback(
    (frameId: string) => {
      remove(frameId);
      forget(frameId);
    },
    [remove, forget],
  );
  const clearAll = useCallback(() => {
    clear();
    reset();
  }, [clear, reset]);

  const blurry = useMemo(() => findBlurry(frames), [frames]);
  const check = useMemo(
    () =>
      checkFrames(
        frames.map((f) => ({ id: f.id, bytes: f.blob.size })),
        blurry.size,
        limits,
        0, // 数枚の追加でも意味があるので「◯枚以上がおすすめ」は出さない
      ),
    [frames, blurry, limits],
  );

  const name = displayTitle(scan);
  const backTo = `/scans/${scan.id}/view`;

  if (room < 1) {
    return (
      <main className="flex min-h-dvh flex-col items-center justify-center gap-4 px-6 text-center text-sm text-mute">
        <AlertIcon width={28} height={28} className="text-amber" />
        <p className="max-w-sm">
          このスキャンは写真が{base}枚あり、上限（{config.limits.maxImages}枚）に達しているので、これ以上追加できません。
        </p>
        <Link to={backTo} className={buttonClass('primary')}>
          3Dデータに戻る
        </Link>
      </main>
    );
  }

  return (
    <div className="mx-auto flex h-dvh max-w-5xl flex-col">
      <header className="pad-safe-t flex items-center justify-between gap-3 px-4 pb-2">
        {phase === 'review' ? (
          <button type="button" onClick={() => setPhase('capture')} className="-ml-2 flex min-h-11 items-center gap-1 px-2 text-sm">
            <BackIcon />
            撮影に戻る
          </button>
        ) : (
          <Link to={backTo} className="-ml-2 flex min-h-11 min-w-0 items-center gap-1 px-2 text-sm">
            <BackIcon className="shrink-0" />
            <span className="truncate">{name}</span>
          </Link>
        )}
        <h1 className="shrink-0 text-base font-semibold tracking-wide">続きを撮る</h1>
      </header>

      {/* 撮影と確認の並べ方は撮影画面（CapturePage）と同じ。 */}
      <div className="grid min-h-0 flex-1 lg:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)] lg:gap-4 lg:px-4 lg:pb-4">
        <div className={`min-h-0 overflow-hidden lg:block lg:rounded-xl lg:border lg:border-line ${phase === 'capture' ? '' : 'hidden'}`}>
          <CaptureView
            frames={frames}
            limits={limits}
            onAdd={add}
            onReview={() => setPhase('review')}
            paused={sending}
            hint={resumeHint(frames.length, limits.maxImages, state)}
            overlay={
              <div className="flex items-start justify-between gap-3">
                <LocalizeChip state={state} onCheck={checkLatest} />
                {info && (
                  <ResumeMap
                    info={info}
                    fixes={fixes}
                    compact
                    className="w-28 shrink-0 rounded-lg border border-line/80 bg-ink/80 p-1"
                  />
                )}
              </div>
            }
          />
        </div>

        <div className={`min-h-0 flex-col lg:flex ${phase === 'review' ? 'flex' : 'hidden'}`}>
          <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-3 lg:px-0">
            <LocalizePanel state={state} info={info} fixes={fixes} onCheck={checkLatest} />
            <FrameGallery frames={frames} blurry={blurry} onRemove={removeFrame} />
            {frames.length > 0 && (
              <div className="mt-3 text-right">
                <button type="button" onClick={clearAll} className="min-h-11 px-2 text-sm text-mute underline underline-offset-4">
                  すべて削除
                </button>
              </div>
            )}
          </div>
          <div className="pad-safe-b border-t border-line bg-panel px-4 pt-3 lg:rounded-xl lg:border lg:pb-4">
            <UploadPanel
              frames={frames}
              check={check}
              limits={limits}
              retentionHours={config.retentionHours}
              keepCompleted={config.keepCompleted}
              pipeline={config.pipeline}
              onSendingChange={setSending}
              resume={{ scanId: scan.id, baseFrameCount: base }}
              onCreated={(result) => {
                clearAll();
                navigate(`/scans/${result.id}`);
              }}
            />
          </div>
        </div>
      </div>
    </div>
  );
}

/** 地図の手掛かり（scene.json）を読む。undefined = 読み込み中 / null = 無い・読めない。 */
function useSceneInfo(scan: Scan): SceneInfo | null | undefined {
  const url = scan.assets.find((a) => a.kind === 'scene_json')?.url;
  const [info, setInfo] = useState<SceneInfo | null | undefined>(url ? undefined : null);
  useEffect(() => {
    if (!url) return;
    const controller = new AbortController();
    fetchSceneInfo(url, controller.signal)
      .then(setInfo)
      .catch(() => {
        if (!controller.signal.aborted) setInfo(null);
      });
    return () => controller.abort();
  }, [url]);
  return info;
}

/** 撮影中の一言。位置合わせの結果に応じて、次にどうすればよいかを伝える。 */
function resumeHint(count: number, max: number, state: LocalizeState): string {
  if (count === 0) return '前回撮った部分が写るように1枚目を撮ると、位置を自動で確かめます。';
  if (count >= max) return '上限に達しました。確認に進んでください。';
  if (state.kind === 'not-found') return '前回撮った部分を写して撮り、左上の表示を押して確かめ直してください。';
  if (state.kind === 'found') return '前回撮れていない側へ、少しずつ移動しながら撮り足しましょう。';
  return '少しずつ移動しながら撮り足しましょう。';
}

/** 位置合わせの状況を短い言葉にする（撮影画面の左上と、確認画面のカードで使う）。 */
function describe(state: LocalizeState): { tone: 'mute' | 'busy' | 'ok' | 'warn' | 'bad'; short: string; long: string } {
  switch (state.kind) {
    case 'idle':
      return {
        tone: 'mute',
        short: '1枚目で前回との位置を確かめます',
        long: 'まだ確かめていません。写真を撮ると、1枚目で自動的に確かめます。',
      };
    case 'checking':
      return { tone: 'busy', short: '位置を確かめています…', long: '位置を確かめています。10〜30秒ほどかかります。' };
    case 'found':
      return {
        tone: 'ok',
        short: `位置が分かりました（${state.result.matches}点一致）`,
        long: `${state.result.message}（${Math.max(1, Math.round(state.result.durationMs / 1000))}秒）`,
      };
    case 'not-found':
      return { tone: 'warn', short: '前回の3Dデータと一致しません', long: state.result.message };
    case 'error':
      return { tone: 'bad', short: '位置を確かめられませんでした', long: state.message };
  }
}

const toneClass = { mute: 'text-mute', busy: 'text-amber', ok: 'text-ok', warn: 'text-amber', bad: 'text-bad' } as const;

function StatusIcon({ tone }: { tone: keyof typeof toneClass }) {
  if (tone === 'busy') return <SpinnerIcon width={16} height={16} className="shrink-0 text-amber" />;
  if (tone === 'warn' || tone === 'bad') return <AlertIcon width={16} height={16} className={`shrink-0 ${toneClass[tone]}`} />;
  return <LocateIcon width={16} height={16} className={`shrink-0 ${toneClass[tone]}`} />;
}

/** 撮影画面の左上に重ねる、位置合わせの状況。押すと最新の写真で確かめ直す。 */
function LocalizeChip({ state, onCheck }: { state: LocalizeState; onCheck?: () => void }) {
  const d = describe(state);
  const retry = onCheck && state.kind !== 'idle' && state.kind !== 'checking';
  return (
    <button
      type="button"
      onClick={onCheck}
      disabled={!onCheck}
      aria-label={retry ? `${d.short}。押すと最新の写真で確かめ直します` : d.short}
      className="pointer-events-auto flex min-h-11 max-w-[15rem] items-center gap-2 rounded-lg bg-ink/85 px-3 py-1.5 text-left text-xs leading-snug disabled:cursor-default"
    >
      <StatusIcon tone={d.tone} />
      <span>
        <span className={d.tone === 'mute' ? 'text-mute' : 'text-paper'}>{d.short}</span>
        {retry && <span className="block text-mute">押すと最新の写真で確かめ直します</span>}
      </span>
    </button>
  );
}

/** 確認画面の上に出す、位置合わせのカード（大きな地図つき）。 */
function LocalizePanel({
  state,
  info,
  fixes,
  onCheck,
}: {
  state: LocalizeState;
  info: SceneInfo | null | undefined;
  fixes: Fix[];
  onCheck?: () => void;
}) {
  const d = describe(state);
  return (
    <section className="mb-4 space-y-2 rounded-xl border border-line bg-panel p-3">
      <div className="flex items-center justify-between gap-2">
        <h2 className="flex items-center gap-1.5 text-sm font-semibold">
          <LocateIcon width={18} height={18} />
          位置合わせ（VPS）
        </h2>
        <Button className="shrink-0 px-3 text-sm whitespace-nowrap" disabled={!onCheck} onClick={onCheck}>
          最新の写真で確認
        </Button>
      </div>
      <p className={`flex items-start gap-1.5 text-sm ${toneClass[d.tone]}`}>
        <StatusIcon tone={d.tone} />
        <span>{d.long}</span>
      </p>
      {info === undefined && <p className="text-xs text-mute">地図を読み込んでいます…</p>}
      {info === null && <p className="text-xs text-mute">前回の撮影位置の情報が無いため、地図は表示できません。</p>}
      {info && <ResumeMap info={info} fixes={fixes} className="mx-auto max-w-xs" />}
      <p className="text-xs leading-relaxed text-mute">
        {'写真に写った景色を前回の3Dデータの特徴点と照らし合わせ、どこから撮ったかを求めています。' +
          '前回撮っていない側へ回り込んで撮り足すと、3Dデータの欠けた部分を埋められます。'}
      </p>
    </section>
  );
}
