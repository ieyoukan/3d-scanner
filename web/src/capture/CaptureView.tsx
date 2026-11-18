// 撮影画面。カメラの映像・シャッター・写真の追加をまとめる。
//
// この部品が扱う状況は3つ:
//   1. カメラを使う前    →「カメラを開始」と「写真を選ぶ」を並べて選んでもらう
//   2. カメラが使える    → 映像とシャッターを出す
//   3. カメラが使えない  → 理由を説明し、「写真を選ぶ」に切り替えてもらう
//
// どの状況でも「写真を選ぶ」は押せる。カメラは便利な入力手段の1つであって必須ではない、
// という作りにしておくと、権限の拒否や HTTP 接続でも行き止まりにならない。
import { useCallback, useEffect, useRef, useState, type ChangeEvent, type ReactNode } from 'react';
import type { Limits } from '../api/types';
import { useCamera } from '../camera/useCamera';
import { Button, buttonClass } from '../ui/Button';
import { AlertIcon, CameraIcon, ForwardIcon, ImagesIcon } from '../ui/icons';
import { frameFromCapture, frameFromFile, FrameError, type Frame } from './frames';
import { captureHint } from './validation';

interface Props {
  frames: readonly Frame[];
  limits: Limits;
  /** 写真を追加する。上限で受け取れなかった枚数を返す。 */
  onAdd: (frames: Frame[]) => number;
  /** 確認画面へ進む。 */
  onReview: () => void;
  /** 映像の上端に重ねる表示（「続きを撮る」の位置合わせの状況など）。 */
  overlay?: ReactNode;
  /** 映像の下に出す一言。省略すると枚数に応じた既定の案内。 */
  hint?: string;
}

export function CaptureView({ frames, limits, onAdd, onReview, overlay, hint }: Props) {
  const camera = useCamera();
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // シャッターを切った瞬間に映像を一瞬白くする（撮れたことが目で分かるように）。
  const [flash, setFlash] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const full = frames.length >= limits.maxImages;

  // 以前にカメラを許可済みなら、ボタンを押させずにすぐ開始する。
  // 未許可のときは勝手に始めない: ページを開いた途端に許可ダイアログが出ると、
  // 何に使うのか分からないまま「拒否」されやすい。説明を読んでから押してもらう。
  const { start } = camera;
  useEffect(() => {
    let cancelled = false;
    // Permissions API は対応していないブラウザもあるので、使えるときだけ使う。
    navigator.permissions
      ?.query({ name: 'camera' as PermissionName })
      .then((status) => {
        if (!cancelled && status.state === 'granted') void start();
      })
      .catch(() => {
        // 問い合わせ自体ができないブラウザ。ボタンから開始してもらう。
      });
    return () => {
      cancelled = true;
    };
  }, [start]);

  /** シャッター。いまの映像を1枚取り込み、一覧に足す。 */
  const shoot = useCallback(async () => {
    if (busy || full) return;
    setBusy(true);
    setNotice(null);
    try {
      const blob = await camera.capture();
      setFlash(true);
      window.setTimeout(() => setFlash(false), 90);
      // 対応端末では短く振動させる（画面を見ていなくても撮れたと分かる）。
      navigator.vibrate?.(12);
      onAdd([await frameFromCapture(blob)]);
    } catch (err) {
      setNotice(err instanceof Error ? err.message : '撮影に失敗しました。');
    } finally {
      setBusy(false);
    }
  }, [busy, full, camera, onAdd]);

  /** 端末から選んだ写真を取り込む。 */
  const pickFiles = useCallback(
    async (event: ChangeEvent<HTMLInputElement>) => {
      const files = Array.from(event.target.files ?? []);
      // 同じファイルをもう一度選べるよう、入力欄を空に戻す。
      event.target.value = '';
      if (files.length === 0) return;
      setBusy(true);
      setNotice(null);
      const loaded: Frame[] = [];
      const problems: string[] = [];
      // 1枚ずつ順番に読む。まとめて並列に展開すると、スマホではメモリが足りなくなることがある。
      for (const file of files) {
        try {
          loaded.push(await frameFromFile(file));
        } catch (err) {
          problems.push(err instanceof FrameError ? err.message : `「${file.name}」を読み込めませんでした。`);
        }
      }
      const rejected = onAdd(loaded);
      if (rejected > 0) problems.push(`上限（${limits.maxImages}枚）を超えた${rejected}枚は追加しませんでした。`);
      setNotice(problems.length > 0 ? problems.join(' ') : null);
      setBusy(false);
    },
    [onAdd, limits.maxImages],
  );

  const ready = camera.state.kind === 'ready';

  return (
    <div className="flex h-full flex-col">
      {/* 映像の領域。写真が主役なので、画面の残り全部を使う。 */}
      <div className="relative min-h-0 flex-1 overflow-hidden bg-black">
        {/*
          playsInline: iOS で全画面再生に切り替わるのを防ぐ。
          muted: 音の無い映像でも、これが無いと自動再生がブロックされるブラウザがある。
          object-cover: 映像の縦横比を保ったまま領域を埋める（端は少し切れる）。
        */}
        <video
          ref={camera.videoRef}
          playsInline
          muted
          className={`h-full w-full object-cover ${ready ? '' : 'invisible'}`}
        />

        {ready && (
          <>
            <FrameCorners />
            <p className="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/75 to-transparent px-4 pb-3 pt-10 text-center text-sm">
              {hint ?? captureHint(frames.length, limits)}
            </p>
          </>
        )}
        {/* 重ねる表示は、カメラが使えないときも出す（写真を選んで送る場合も位置合わせはできる）。 */}
        {overlay && <div className="pointer-events-none absolute inset-x-0 top-0 z-10 p-3">{overlay}</div>}
        {flash && <div className="pointer-events-none absolute inset-0 bg-white/70" />}

        {camera.state.kind === 'idle' && (
          <Overlay>
            <CameraIcon width={36} height={36} className="text-mute" />
            <p className="max-w-xs text-center text-sm text-mute">
              対象のまわりを少しずつ移動しながら、たくさん撮ります。撮影にはカメラの使用許可が必要です。
            </p>
            <Button variant="primary" onClick={() => void camera.start()}>
              <CameraIcon />
              カメラを開始
            </Button>
            <Button onClick={() => fileInput.current?.click()}>
              <ImagesIcon />
              写真を選ぶ
            </Button>
          </Overlay>
        )}

        {camera.state.kind === 'starting' && (
          <Overlay>
            <p className="text-sm text-mute">カメラを準備しています…</p>
          </Overlay>
        )}

        {camera.state.kind === 'unavailable' && (
          <Overlay>
            <AlertIcon width={32} height={32} className="text-amber" />
            <p className="max-w-xs text-center text-sm">{camera.state.failure.message}</p>
            <Button variant="primary" onClick={() => fileInput.current?.click()}>
              <ImagesIcon />
              写真を選ぶ
            </Button>
            {/* HTTPS でない・非対応の場合は、やり直しても結果が変わらないのでボタンを出さない。 */}
            {camera.state.failure.problem !== 'insecure' && camera.state.failure.problem !== 'unsupported' && (
              <Button onClick={() => void camera.start()}>もう一度カメラを試す</Button>
            )}
          </Overlay>
        )}
      </div>

      {notice && (
        <p role="alert" className="flex items-start gap-2 border-t border-line bg-panel px-4 py-2 text-sm text-amber">
          <AlertIcon className="mt-0.5 shrink-0" />
          <span>{notice}</span>
        </p>
      )}

      {/* 操作は下にまとめる。片手で持ったとき、親指が届く場所。 */}
      <div className="pad-safe-b grid grid-cols-[1fr_auto_1fr] items-center gap-3 border-t border-line bg-panel px-4 pt-3">
        <div className="justify-self-start">
          {/*
            <input type="file"> は見た目を変えにくいので隠し、ボタンから click() で開く。
            accept="image/*" で写真だけを選ばせ、multiple で複数選択を許す。
            capture 属性は付けない: 付けるとカメラアプリが直接開き、撮影済みの写真を選べなくなる端末がある。
          */}
          <input ref={fileInput} type="file" accept="image/*" multiple hidden onChange={(e) => void pickFiles(e)} />
          <button
            type="button"
            className={buttonClass('secondary', 'px-3')}
            onClick={() => fileInput.current?.click()}
            disabled={busy || full}
          >
            <ImagesIcon />
            <span className="text-sm">追加</span>
          </button>
        </div>

        <button
          type="button"
          onClick={() => void shoot()}
          disabled={!ready || busy || full}
          aria-label="撮影する"
          className="group grid size-[72px] place-items-center rounded-full border-[3px] border-paper disabled:border-line"
        >
          <span className="size-[56px] rounded-full bg-paper transition-transform group-active:scale-90 group-disabled:bg-line" />
        </button>

        <div className="justify-self-end">
          <button
            type="button"
            className={buttonClass(frames.length >= limits.minImages ? 'primary' : 'secondary', 'px-3')}
            onClick={onReview}
            disabled={frames.length === 0}
          >
            <span className="tabular text-sm">確認 {frames.length}枚</span>
            <ForwardIcon />
          </button>
        </div>
      </div>
    </div>
  );
}

/** 映像の上に重ねる、中央寄せの案内。 */
function Overlay({ children }: { children: React.ReactNode }) {
  return <div className="absolute inset-0 flex flex-col items-center justify-center gap-4 p-6">{children}</div>;
}

/** 構図の目安になる四隅の印。被写体をこの内側に収めると、隣の写真との重なりを取りやすい。 */
function FrameCorners() {
  const corner = 'absolute size-6 border-paper/80';
  return (
    <div className="pointer-events-none absolute inset-[12%]">
      <span className={`${corner} left-0 top-0 border-l-2 border-t-2`} />
      <span className={`${corner} right-0 top-0 border-r-2 border-t-2`} />
      <span className={`${corner} bottom-0 left-0 border-b-2 border-l-2`} />
      <span className={`${corner} bottom-0 right-0 border-b-2 border-r-2`} />
    </div>
  );
}
