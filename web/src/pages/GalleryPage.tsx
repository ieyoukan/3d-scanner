// 「/gallery」の画面。これまでに作った3Dデータの一覧。
//
// 完成したもの（成功・一部成功）はサーバーがギャラリーに保存し、保存期限で消さない。
// 処理中や失敗したものも一覧に出す（あとから進み具合や失敗の理由を確かめられるように）。
// 失敗したものは保存期限が来ると消える。
import { useCallback, useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { fetchGallery } from '../api/client';
import type { GalleryItem } from '../api/types';
import { destination, displayTitle, formatLabels, groupByDay, statusBadge, type Tone } from '../gallery/gallery';
import { Button, buttonClass } from '../ui/Button';
import { BackIcon, CameraIcon, CubeIcon, SpinnerIcon } from '../ui/icons';

type Load =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'ready'; items: GalleryItem[]; next?: string; more: 'idle' | 'loading' | 'error' };

export function GalleryPage() {
  const [load, setLoad] = useState<Load>({ kind: 'loading' });

  const loadFirst = useCallback((signal?: AbortSignal) => {
    setLoad({ kind: 'loading' });
    fetchGallery(undefined, signal)
      .then((page) => setLoad({ kind: 'ready', items: page.items, next: page.nextCursor, more: 'idle' }))
      .catch((err: unknown) => {
        if (signal?.aborted) return;
        setLoad({ kind: 'error', message: err instanceof Error ? err.message : '読み込めませんでした。' });
      });
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    loadFirst(controller.signal);
    return () => controller.abort();
  }, [loadFirst]);

  const loadMore = async () => {
    if (load.kind !== 'ready' || !load.next) return;
    const current = load;
    setLoad({ ...current, more: 'loading' });
    try {
      const page = await fetchGallery(current.next);
      // 読み込みの間に新しいスキャンが増えても、カーソルの位置から先を読むので重複はしない。
      setLoad({ kind: 'ready', items: [...current.items, ...page.items], next: page.nextCursor, more: 'idle' });
    } catch {
      setLoad({ ...current, more: 'error' });
    }
  };

  return (
    <div className="mx-auto flex min-h-dvh max-w-5xl flex-col">
      <header className="pad-safe-t flex items-center justify-between gap-3 px-4 pb-2">
        <Link to="/" className="-ml-2 flex min-h-11 items-center gap-1 px-2 text-sm">
          <BackIcon />
          撮影画面
        </Link>
        <h1 className="text-base font-semibold tracking-wide">ギャラリー</h1>
        {/* 見出しを中央に置くための、左のリンクと同じ幅の空き */}
        <span className="w-[5.5rem]" aria-hidden="true" />
      </header>

      <main className="flex-1 px-4 pb-8">
        {load.kind === 'loading' && (
          <p className="flex items-center gap-2 py-10 text-sm text-mute">
            <SpinnerIcon />
            読み込んでいます…
          </p>
        )}

        {load.kind === 'error' && (
          <div className="space-y-3 py-10 text-sm">
            <p className="text-bad">{load.message}</p>
            <Button onClick={() => loadFirst()}>もう一度試す</Button>
          </div>
        )}

        {load.kind === 'ready' && load.items.length === 0 && <Empty />}

        {load.kind === 'ready' && load.items.length > 0 && (
          <>
            {groupByDay(load.items).map((group) => (
              <section key={group.label} className="mt-4 first:mt-1">
                <h2 className="mb-2 text-xs font-semibold text-mute">{group.label}</h2>
                <ul className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
                  {group.items.map((item) => (
                    <li key={item.id}>
                      <Card item={item} />
                    </li>
                  ))}
                </ul>
              </section>
            ))}

            {load.next && (
              <div className="mt-6 flex flex-col items-center gap-2">
                {load.more === 'error' && <p className="text-sm text-bad">続きを読み込めませんでした。</p>}
                <Button disabled={load.more === 'loading'} onClick={() => void loadMore()}>
                  {load.more === 'loading' && <SpinnerIcon />}
                  もっと見る
                </Button>
              </div>
            )}
          </>
        )}
      </main>
    </div>
  );
}

const toneClass: Record<Tone, string> = {
  neutral: 'text-mute',
  active: 'text-amber',
  good: 'text-ok',
  warn: 'text-amber',
  bad: 'text-bad',
};

function Card({ item }: { item: GalleryItem }) {
  const badge = statusBadge(item.status);
  const formats = formatLabels(item.assetKinds);
  return (
    <Link
      to={destination(item)}
      // ビューアの「戻る」でギャラリーに戻れるよう、どこから来たかを渡す。
      state={{ from: 'gallery' }}
      className="group block overflow-hidden rounded-xl border border-line bg-panel transition-colors hover:border-mute"
    >
      <div className="relative aspect-[4/3] bg-raised">
        {item.thumbnailUrl ? (
          // loading="lazy": 画面に近づくまで画像を読まない。一覧が長くても最初の表示が速い。
          <img src={item.thumbnailUrl} alt="" loading="lazy" decoding="async" className="size-full object-cover" />
        ) : (
          <div className="grid size-full place-items-center text-mute">
            <CubeIcon width={28} height={28} />
          </div>
        )}
        {badge && (
          <span className={`absolute top-2 left-2 rounded-md bg-ink/85 px-1.5 py-0.5 text-xs font-semibold ${toneClass[badge.tone]}`}>
            {badge.label}
          </span>
        )}
        {item.baseFrameCount > 0 && (
          // 「続きを撮る」で足した枚数。同じ対象を撮り足した新しい版だと分かるように。
          <span className="tabular absolute top-2 right-2 rounded-md bg-ink/85 px-1.5 py-0.5 text-xs text-paper">
            追加 +{item.frameCount - item.baseFrameCount}枚
          </span>
        )}
      </div>
      <div className="space-y-0.5 px-2.5 py-2">
        <p className="truncate text-sm font-semibold">{displayTitle(item)}</p>
        <p className="tabular truncate text-xs text-mute">
          {item.frameCount}枚{formats.length > 0 && ` · ${formats.join('・')}`}
        </p>
      </div>
    </Link>
  );
}

function Empty() {
  return (
    <div className="flex flex-col items-center gap-4 py-16 text-center">
      <CubeIcon width={36} height={36} className="text-mute" />
      <div className="space-y-1">
        <p className="font-semibold">まだ3Dデータがありません</p>
        <p className="text-sm text-mute">撮影して作った3Dデータは、ここに保存されます。</p>
      </div>
      <Link to="/" className={buttonClass('primary')}>
        <CameraIcon />
        撮影する
      </Link>
    </div>
  );
}
