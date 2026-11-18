// 撮った写真の一覧（確認画面）。不要な写真を1枚ずつ削除できる。
//
// 表示するのは 320px のサムネイルだけ。タイルを押すと、その1枚だけを大きく表示して
// ピントや構図を確かめられる（大きい画像はそのときだけ作り、閉じたら解放する）。
import { useEffect, useMemo, useState } from 'react';
import { Button } from '../ui/Button';
import { CloseIcon, TrashIcon } from '../ui/icons';
import type { Frame, FrameSource } from './frames';

const sourceLabel: Record<FrameSource, string> = { camera: '・カメラ', file: '・ファイル', video: '・動画' };

interface Props {
  frames: readonly Frame[];
  /** ぼけている可能性がある写真の id。 */
  blurry: ReadonlySet<string>;
  onRemove: (id: string) => void;
}

export function FrameGallery({ frames, blurry, onRemove }: Props) {
  const [openId, setOpenId] = useState<string | null>(null);
  const open = frames.find((f) => f.id === openId) ?? null;

  if (frames.length === 0) {
    return <p className="px-4 py-10 text-center text-sm text-mute">まだ写真がありません。</p>;
  }

  return (
    <>
      {/* 列の数は幅に合わせて増やす（スマホで3列、広い画面では自動で増える）。 */}
      <ul className="grid grid-cols-[repeat(auto-fill,minmax(104px,1fr))] gap-1.5">
        {frames.map((frame, index) => (
          <li key={frame.id} className="relative aspect-square overflow-hidden rounded-md bg-panel">
            <button
              type="button"
              className="block h-full w-full"
              onClick={() => setOpenId(frame.id)}
              aria-label={`${index + 1}枚目を拡大`}
            >
              <img
                src={frame.thumbUrl}
                alt=""
                // 画面外のサムネイルは、近づくまで読み込み・展開を後回しにする。
                loading="lazy"
                decoding="async"
                className="h-full w-full object-cover"
              />
            </button>
            <span className="tabular pointer-events-none absolute left-1 top-1 rounded bg-black/65 px-1.5 text-xs">
              {index + 1}
            </span>
            {blurry.has(frame.id) && (
              <span className="pointer-events-none absolute bottom-1 left-1 rounded bg-amber px-1.5 text-xs font-semibold text-ink">
                ぼけ?
              </span>
            )}
            {/* 削除ボタンは見た目は小さいが、押せる範囲は 36px 四方ある。 */}
            <button
              type="button"
              onClick={() => onRemove(frame.id)}
              aria-label={`${index + 1}枚目を削除`}
              className="absolute right-0 top-0 grid size-9 place-items-center"
            >
              <span className="grid size-7 place-items-center rounded-full bg-black/65">
                <CloseIcon width={16} height={16} />
              </span>
            </button>
          </li>
        ))}
      </ul>

      {open && (
        <FramePreview
          frame={open}
          index={frames.indexOf(open)}
          blurry={blurry.has(open.id)}
          onClose={() => setOpenId(null)}
          onRemove={() => {
            onRemove(open.id);
            setOpenId(null);
          }}
        />
      )}
    </>
  );
}

interface PreviewProps {
  frame: Frame;
  index: number;
  blurry: boolean;
  onClose: () => void;
  onRemove: () => void;
}

/** 1枚を大きく表示するダイアログ。 */
function FramePreview({ frame, index, blurry, onClose, onRemove }: PreviewProps) {
  // 送信用の本体から表示用の URL を作る。この部品が消えるときに解放する。
  const url = useMemo(() => URL.createObjectURL(frame.blob), [frame.blob]);
  useEffect(() => () => URL.revokeObjectURL(url), [url]);

  // Esc キーで閉じられるようにする。
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  return (
    <div role="dialog" aria-modal="true" aria-label={`${index + 1}枚目の写真`} className="fixed inset-0 z-20 flex flex-col bg-ink">
      <div className="pad-safe-t flex items-center justify-between px-4 pb-2">
        <p className="tabular text-sm text-mute">
          {index + 1}枚目・{frame.width}×{frame.height}
          {sourceLabel[frame.source]}
        </p>
        <button type="button" onClick={onClose} aria-label="閉じる" className="grid size-11 place-items-center">
          <CloseIcon />
        </button>
      </div>
      <div className="min-h-0 flex-1">
        <img src={url} alt="" className="h-full w-full object-contain" />
      </div>
      {blurry && (
        <p className="px-4 pt-3 text-sm text-amber">
          他の写真に比べて輪郭がはっきりしていません。ぶれやピンぼけなら削除をおすすめします。
        </p>
      )}
      <div className="pad-safe-b flex gap-3 px-4 pt-3">
        <Button variant="danger" className="flex-1" onClick={onRemove}>
          <TrashIcon />
          この写真を削除
        </Button>
        <Button className="flex-1" onClick={onClose}>
          閉じる
        </Button>
      </div>
    </div>
  );
}
