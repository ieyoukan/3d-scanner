// 「/」の画面。撮影 → 確認 → 送信 までを1ページで行う。
//
// このページが持つ状態は2つだけ:
//   frames  撮った写真の一覧（useFrames）
//   phase   いま「撮影」と「確認」のどちらを見ているか
//
// 送信が受け付けられたら /scans/<id> に移動する。以降の進行状況はそちらの画面が担当する。
import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import type { ServerConfig } from '../api/types';
import { CaptureView } from '../capture/CaptureView';
import { FrameGallery } from '../capture/FrameGallery';
import { findBlurry } from '../capture/sharpness';
import { useFrames } from '../capture/useFrames';
import { checkFrames } from '../capture/validation';
import { Button } from '../ui/Button';
import { BackIcon, GridIcon, InfoIcon } from '../ui/icons';
import { UploadPanel } from '../upload/UploadPanel';

type Phase = 'capture' | 'review';

interface Props {
  config: ServerConfig;
}

export function CapturePage({ config }: Props) {
  const { limits } = config;
  const navigate = useNavigate();
  const { frames, add, remove, clear } = useFrames(limits.maxImages);
  const [phase, setPhase] = useState<Phase>('capture');
  // 送信中は撮影を止める（自動撮影が、送信中の一覧に写真を足さないように）。
  const [sending, setSending] = useState(false);
  const [showGuide, setShowGuide] = useState(false);

  // frames から計算できる値は state にせず、その場で求める（同じ情報を2か所に持つと食い違う）。
  // useMemo は「frames が変わったときだけ計算し直す」ための指定。
  const blurry = useMemo(() => findBlurry(frames), [frames]);
  const check = useMemo(
    () =>
      checkFrames(
        frames.map((f) => ({ id: f.id, bytes: f.blob.size })),
        blurry.size,
        limits,
      ),
    [frames, blurry, limits],
  );

  return (
    <div className="mx-auto flex h-dvh max-w-5xl flex-col">
      <header className="pad-safe-t flex items-center justify-between gap-3 px-4 pb-2">
        {phase === 'review' ? (
          <button type="button" onClick={() => setPhase('capture')} className="-ml-2 flex min-h-11 items-center gap-1 px-2 text-sm">
            <BackIcon />
            撮影に戻る
          </button>
        ) : (
          <h1 className="text-base font-semibold tracking-wide">3D Scanner</h1>
        )}
        <div className="-mr-2 flex items-center">
          <button
            type="button"
            onClick={() => setShowGuide(true)}
            className="flex min-h-11 items-center gap-1.5 px-2 text-sm text-mute"
          >
            <InfoIcon />
            撮り方
          </button>
          <Link to="/gallery" className="flex min-h-11 items-center gap-1.5 px-2 text-sm">
            <GridIcon />
            ギャラリー
          </Link>
        </div>
      </header>

      {/*
        狭い画面: phase に応じて撮影か確認のどちらか一方を表示する。
        広い画面（lg 以上）: 左に撮影、右に確認を並べて両方表示する。
        どちらの場合も CaptureView は消さずに hidden で隠すだけにする。
        消してしまうと、確認から戻るたびにカメラを起動し直すことになる。
      */}
      <div className="grid min-h-0 flex-1 lg:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)] lg:gap-4 lg:px-4 lg:pb-4">
        <div className={`min-h-0 overflow-hidden lg:block lg:rounded-xl lg:border lg:border-line ${phase === 'capture' ? '' : 'hidden'}`}>
          <CaptureView frames={frames} limits={limits} onAdd={add} onReview={() => setPhase('review')} paused={sending} />
        </div>

        <div className={`min-h-0 flex-col lg:flex ${phase === 'review' ? 'flex' : 'hidden'}`}>
          <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-3 lg:px-0">
            <FrameGallery frames={frames} blurry={blurry} onRemove={remove} />
            {frames.length > 0 && (
              <div className="mt-3 text-right">
                <button type="button" onClick={clear} className="min-h-11 px-2 text-sm text-mute underline underline-offset-4">
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
              onCreated={(result) => {
                // 送信済みの写真はもう不要。メモリを空けてから移動する。
                clear();
                navigate(`/scans/${result.id}`);
              }}
            />
          </div>
        </div>
      </div>

      {showGuide && <CaptureGuide minImages={limits.minImages} onClose={() => setShowGuide(false)} />}
    </div>
  );
}

/** 撮り方の説明。なぜそう撮るのか（SfM の都合）を一言ずつ添える。 */
function CaptureGuide({ minImages, onClose }: { minImages: number; onClose: () => void }) {
  const tips: { title: string; why: string }[] = [
    {
      title: '対象のまわりを、少しずつ移動しながら撮る',
      why: '同じ場所で向きだけ変えても奥行きは分かりません。カメラの位置が動くことで、立体の手掛かり（視差）が生まれます。',
    },
    {
      title: '隣の写真と7割ほど重ねる',
      why: '写真どうしで「同じ点」を見つけて位置を合わせます。重なりが少ないと、つながりが切れて失敗します。',
    },
    {
      title: '高さを変えて、もう1周',
      why: '上からと下からの写真があると、天面や側面の隠れた部分まで復元できます。',
    },
    {
      title: '光る面・透明な物・無地の壁は苦手',
      why: '見る角度で見た目が変わる面や、模様の無い面では「同じ点」を見つけられません。',
    },
    {
      title: 'ぶれないように、止まってから撮る',
      why: 'ぶれた写真からは輪郭が取れず、位置合わせに使えません。暗い場所は特に注意。',
    },
    {
      title: '「自動撮影」なら、止まるたびに撮れる',
      why: 'ゆっくり移動しては止まる、を繰り返すと、景色が十分変わっていれば自動で撮ります。撮り忘れや撮りすぎが減ります。',
    },
  ];
  return (
    <div role="dialog" aria-modal="true" aria-label="撮り方" className="fixed inset-0 z-20 flex flex-col bg-ink">
      <div className="pad-safe-t flex items-center justify-between px-4 pb-2">
        <h2 className="text-base font-semibold">撮り方</h2>
        <Button onClick={onClose}>閉じる</Button>
      </div>
      <div className="mx-auto w-full max-w-xl flex-1 overflow-y-auto px-4 pb-8">
        <p className="mb-5 text-sm text-mute">
          最低{minImages}枚、できれば20〜60枚。対象を中心に、円を描くように歩きながら撮るのが基本です。
        </p>
        <ol className="space-y-5">
          {tips.map((tip, i) => (
            <li key={tip.title} className="grid grid-cols-[1.75rem_1fr] gap-x-3">
              <span className="tabular row-span-2 pt-0.5 text-sm text-amber">{i + 1}</span>
              <p className="font-semibold">{tip.title}</p>
              <p className="text-sm text-mute">{tip.why}</p>
            </li>
          ))}
        </ol>
      </div>
    </div>
  );
}
