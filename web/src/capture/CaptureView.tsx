// 撮影画面。カメラの映像・シャッター・写真の追加をまとめる。
//
// この部品が扱う状況は3つ:
//   1. カメラを使う前    →「カメラを開始」と「写真を選ぶ」を並べて選んでもらう
//   2. カメラが使える    → 映像とシャッターを出す
//   3. カメラが使えない  → 理由を説明し、「写真を選ぶ」に切り替えてもらう
//
// どの状況でも「写真を選ぶ」は押せる。カメラは便利な入力手段の1つであって必須ではない、
// という作りにしておくと、権限の拒否や HTTP 接続でも行き止まりにならない。
//
// 撮り方の選択（カメラが使えるときに出す）:
//   自動撮影  映像を見て、止まった瞬間に、前の写真から景色が十分変わっていれば自動で撮る（autoCapture.ts）。
//             重なりの足りない写真やぶれた写真を減らし、「一部しか復元できない」を防ぐ。
//   静止画    対応端末では、映像の1コマではなくカメラの静止画で撮る（解像度が高く、ぶれ・ノイズが少ない）。
//             撮れなかったら映像の1コマに切り替え、そのことを伝える。
import { useCallback, useEffect, useRef, useState, type ChangeEvent, type ReactNode } from 'react';
import type { Limits } from '../api/types';
import { useCamera } from '../camera/useCamera';
import { Button, buttonClass } from '../ui/Button';
import { AlertIcon, CameraIcon, ForwardIcon, ImagesIcon } from '../ui/icons';
import { decide, initialAutoState, markCaptured, signatureOf, type AutoAdvice, type AutoState } from './autoCapture';
import { frameFromCapture, frameFromFile, frameFromPhoto, FrameError, type Frame } from './frames';
import { captureHint } from './validation';

/** 自動撮影の判断の間隔（ミリ秒）。1秒に5回見る。 */
const autoIntervalMs = 200;
/** 判断に使う映像の大きさ（4:3）。 */
const sampleWidth = 64;
const sampleHeight = 48;

/** 端末に覚えておく設定（次に開いたときも同じにする）。 */
function usePreference(key: string, initial: boolean): [boolean, (v: boolean) => void] {
  const [value, setValue] = useState(() => {
    try {
      const saved = window.localStorage.getItem(key);
      return saved === null ? initial : saved === '1';
    } catch {
      return initial;
    }
  });
  const set = useCallback(
    (v: boolean) => {
      setValue(v);
      try {
        window.localStorage.setItem(key, v ? '1' : '0');
      } catch {
        // 保存できない（プライベートモードなど）ときは、この画面の間だけ覚える。
      }
    },
    [key],
  );
  return [value, set];
}

/** 自動撮影の案内。 */
function autoHint(advice: AutoAdvice | null, tooFar: boolean): string {
  if (tooFar) return '前の写真から離れすぎました。少し戻って、間を埋める写真も撮ると確実です。';
  switch (advice) {
    case 'flat':
      return '模様のある所を写してください。無地だと位置を合わせられません。';
    case 'move':
      return '少し横へ移動してください。止まると自動で撮ります。';
    case 'capture':
      return '撮りました。そのまま少しずつ移動してください。';
    default:
      return 'ゆっくり移動して、止まると自動で撮ります。';
  }
}

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
  /**
   * true のあいだは撮影しない（写真の送信中など）。自動撮影も止め、シャッターと「追加」も押せなくする。
   * 送信中に撮った写真は送られず、送信が終わると一覧ごと消えてしまうため。
   */
  paused?: boolean;
}

export function CaptureView({ frames, limits, onAdd, onReview, overlay, hint, paused = false }: Props) {
  const camera = useCamera();
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // シャッターを切った瞬間に映像を一瞬白くする（撮れたことが目で分かるように）。
  const [flash, setFlash] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const full = frames.length >= limits.maxImages;

  const [auto, setAuto] = usePreference('capture.auto', false);
  const [still, setStill] = usePreference('capture.still', true);
  // 静止画で撮れなかったら、この画面の間は映像の1コマに切り替える（写真の寸法が混ざらないように）。
  const stillBroken = useRef(false);
  const stillAvailable = camera.state.kind === 'ready' && camera.state.still && !stillBroken.current;
  const [advice, setAdvice] = useState<AutoAdvice | null>(null);
  const [tooFar, setTooFar] = useState(false);

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

  // 自動撮影のループから最新の値を読むための控え（ループを作り直さずに済むように）。
  const busyRef = useRef(false);
  const fullRef = useRef(full);
  fullRef.current = full;
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  /** 1枚撮って一覧に足す。静止画が使えれば静止画、だめなら映像の1コマ。撮れたら true。 */
  const shoot = useCallback(async (): Promise<boolean> => {
    if (busyRef.current || fullRef.current || pausedRef.current) return false;
    busyRef.current = true;
    setBusy(true);
    setNotice(null);
    try {
      let frame: Frame;
      if (still && stillAvailable) {
        try {
          frame = await frameFromPhoto(await camera.takePhoto());
        } catch {
          stillBroken.current = true;
          setNotice('静止画で撮れなかったため、映像から切り出す方法に切り替えました。');
          frame = await frameFromCapture(await camera.capture());
        }
      } else {
        frame = await frameFromCapture(await camera.capture());
      }
      setFlash(true);
      window.setTimeout(() => setFlash(false), 90);
      // 対応端末では短く振動させる（画面を見ていなくても撮れたと分かる）。
      navigator.vibrate?.(12);
      onAdd([frame]);
      return true;
    } catch (err) {
      setNotice(err instanceof Error ? err.message : '撮影に失敗しました。');
      return false;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }, [camera, onAdd, still, stillAvailable]);

  // 自動撮影のループからは最新の shoot を呼ぶ（設定を切り替えても、ループと判断の記録を作り直さない）。
  const shootRef = useRef(shoot);
  shootRef.current = shoot;

  // 自動撮影: 一定間隔で映像を見て、撮るべき瞬間なら撮る。
  const ready = camera.state.kind === 'ready';
  const { sample, videoRef } = camera;
  useEffect(() => {
    if (!auto || !ready || paused) {
      setAdvice(null);
      return;
    }
    let state: AutoState = initialAutoState;
    let tooFarTimer: number | undefined;
    const timer = window.setInterval(() => {
      // 撮影中（静止画は時間がかかる）はプレビューが止まることがあるので、判断しない。
      if (busyRef.current || fullRef.current || pausedRef.current || document.hidden) return;
      // 撮影画面が隠れている（狭い画面で「確認」を開いている）ときは撮らない。
      // 確認から戻るたびにカメラを起動し直さずに済むよう、この部品は消さずに隠しているので、ループは動き続ける。
      // 戻ってきたら判断をやり直す（隠れる前の「止まっていた」記録で撮らないように）。
      if (!isShown(videoRef.current)) {
        state = initialAutoState;
        return;
      }
      const gray = sample(sampleWidth, sampleHeight);
      if (!gray) return;
      const sig = signatureOf(gray, sampleWidth, sampleHeight);
      const now = performance.now();
      const d = decide(state, sig, now);
      state = d.state;
      setAdvice(d.advice);
      if (d.advice !== 'capture') return;
      // 撮った時点で記録する（撮影に時間がかかっても、同じ景色をもう一度撮らない）。
      state = markCaptured(state, sig, now);
      if (d.tooFar) {
        setTooFar(true);
        window.clearTimeout(tooFarTimer);
        tooFarTimer = window.setTimeout(() => setTooFar(false), 3000);
      }
      void shootRef.current();
    }, autoIntervalMs);
    return () => {
      window.clearInterval(timer);
      window.clearTimeout(tooFarTimer);
    };
  }, [auto, ready, paused, sample, videoRef]);

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
              {paused
                ? '写真を送信しています。送信が終わるまで撮影を止めています。'
                : auto
                  ? autoHint(advice, tooFar)
                  : (hint ?? captureHint(frames.length, limits))}
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

      {/* 撮り方の選択。カメラが使えるときだけ出す。 */}
      {ready && (
        <div className="flex items-center justify-center gap-2 border-t border-line bg-panel px-4 pt-2.5">
          <Toggle on={auto} onChange={setAuto} label="自動撮影" />
          {camera.state.kind === 'ready' && camera.state.still && (
            <Toggle on={still && !stillBroken.current} onChange={setStill} label="静止画" />
          )}
        </div>
      )}

      {/* 操作は下にまとめる。片手で持ったとき、親指が届く場所。 */}
      <div className="pad-safe-b grid grid-cols-[1fr_auto_1fr] items-center gap-3 bg-panel px-4 pt-3">
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
            disabled={busy || full || paused}
          >
            <ImagesIcon />
            <span className="text-sm">追加</span>
          </button>
        </div>

        <button
          type="button"
          onClick={() => void shoot()}
          disabled={!ready || busy || full || paused}
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

/** 要素が画面に出ているか（自分か祖先が display: none なら false）。 */
function isShown(el: HTMLElement | null): boolean {
  return el !== null && el.getClientRects().length > 0;
}

/** 撮り方の切り替え（押すたびに入・切）。 */
function Toggle({ on, onChange, label }: { on: boolean; onChange: (v: boolean) => void; label: string }) {
  return (
    <button
      type="button"
      aria-pressed={on}
      onClick={() => onChange(!on)}
      className={`flex min-h-9 items-center gap-1.5 rounded-full border px-3 text-sm ${
        on ? 'border-amber text-amber' : 'border-line text-mute'
      }`}
    >
      <span className={`size-2 rounded-full ${on ? 'bg-amber' : 'bg-line'}`} aria-hidden="true" />
      {label}
    </button>
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
