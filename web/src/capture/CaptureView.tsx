// 撮影画面。カメラの映像・シャッター・写真の追加・動画の取り込みをまとめる。
//
// この部品が扱う状況は3つ:
//   1. カメラを使う前    →「動画から作る」「カメラを開始」「写真を選ぶ」を並べて選んでもらう
//   2. カメラが使える    → 映像とシャッターを出す
//   3. カメラが使えない  → 理由を説明し、「動画から作る」か「写真を選ぶ」に切り替えてもらう
//
// どの状況でも動画と写真は選べる。カメラは便利な入力手段の1つであって必須ではない、
// という作りにしておくと、権限の拒否や HTTP 接続でも行き止まりにならない。
//
// 撮り方は「動画」と「写真」の2つ（既定は動画。端末に覚える）:
//   動画で撮る（liveVideo.ts）: 録画ボタンを押して、対象のまわりを歩きながら1周し、停止する。
//     撮っている間にカメラの映像を見て、1/3 秒ごとの区間で一番くっきりしたコマを写真にしていく。
//     停止したら写真の一覧に足し、確認画面へ進む。
//   写真で撮る: シャッターで1枚ずつ（自動撮影・静止画を選べる）。
//
// 動画を選ぶ（video.ts）:
//   撮ってある動画を選ぶと、映像の領域で動画を再生しながら、1秒に3枚、ぶれの少ないコマを選んで
//   写真の一覧に足し、確認画面へ進む。読んでいる間は撮影しない（自動撮影も止める）。
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
import { AlertIcon, CameraIcon, ForwardIcon, ImagesIcon, VideoIcon } from '../ui/icons';
import { decide, initialAutoState, markCaptured, signatureOf, type AutoAdvice, type AutoState } from './autoCapture';
import { frameFromCapture, frameFromFile, frameFromPhoto, FrameError, type Frame } from './frames';
import { captureHint } from './validation';
import { startLiveRecording, type LiveProgress, type LiveRecording } from './liveVideo';
import { framesFromVideo, type VideoProgress } from './video';

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
    case 'stop':
    case 'capture':
      return '止めてください。止まると撮ります。';
    default:
      return 'バーがいっぱいになるまで、横へ少し移動してください。';
  }
}

/**
 * 自動撮影の進み具合。左から「移動 → 止める → 撮る」の順に進むことが目で分かるようにする。
 *   移動  前の写真から景色がどれだけ変わったか（白いバーが伸びる）
 *   止める  十分変わった（バーが黄色になる）。止まれば撮る
 */
function AutoMeter({ advice, progress }: { advice: AutoAdvice | null; progress: number }) {
  if (advice === null || advice === 'flat') return null;
  const ready = advice !== 'move';
  return (
    <div className="mx-auto mb-2 flex max-w-xs items-center gap-2 text-xs">
      <span className={ready ? 'text-mute' : 'text-paper'}>移動</span>
      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-white/25">
        <div
          className={`h-full rounded-full transition-[width] duration-150 ${ready ? 'bg-amber' : 'bg-paper'}`}
          style={{ width: `${Math.round((ready ? 1 : progress) * 100)}%` }}
        />
      </div>
      <span className={ready ? 'font-semibold text-amber' : 'text-mute'}>止める</span>
    </div>
  );
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

  // 動画から写真を選んでいる最中の状態。importFile が入ると読み込みを始める（下の useEffect）。
  const videoInput = useRef<HTMLInputElement>(null);
  const importVideoRef = useRef<HTMLVideoElement>(null);
  const [importFile, setImportFile] = useState<File | null>(null);
  const [importProgress, setImportProgress] = useState<VideoProgress | null>(null);
  const importing = importFile !== null;

  // 撮り方（true: 動画で撮る / false: 写真で撮る）と、動画で撮っている最中の状態。
  const [videoMode, setVideoMode] = usePreference('capture.video', true);
  const recorderRef = useRef<LiveRecording | null>(null);
  const [recording, setRecording] = useState<LiveProgress | null>(null);
  // 停止してから、最後の写真ができあがるまで。
  const [finishing, setFinishing] = useState(false);
  const isRecording = recording !== null;

  // 送信中・動画を読んでいる間・動画で撮っている間は、ほかの方法で撮らない。
  const blocked = paused || importing || isRecording || finishing;

  const [auto, setAuto] = usePreference('capture.auto', false);
  const [still, setStill] = usePreference('capture.still', true);
  // 静止画で撮れなかったら、この画面の間は映像の1コマに切り替える（写真の寸法が混ざらないように）。
  const stillBroken = useRef(false);
  const stillAvailable = camera.state.kind === 'ready' && camera.state.still && !stillBroken.current;
  const [advice, setAdvice] = useState<AutoAdvice | null>(null);
  const [progress, setProgress] = useState(0);
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
  const pausedRef = useRef(blocked);
  pausedRef.current = blocked;

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
    if (!auto || videoMode || !ready || blocked) {
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
      // 細かく変えると描き直しが増えるので、5% 刻みにする。
      setProgress(Math.round(d.progress * 20) / 20);
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
  }, [auto, videoMode, ready, blocked, sample, videoRef]);

  // 動画の読み込みから、最新の値を読むための控え（値が変わるたびに読み込みをやり直さないように）。
  const onAddRef = useRef(onAdd);
  onAddRef.current = onAdd;
  const onReviewRef = useRef(onReview);
  onReviewRef.current = onReview;
  const roomRef = useRef(limits.maxImages - frames.length);
  roomRef.current = limits.maxImages - frames.length;

  // 動画から写真を選ぶ。再生に使う <video> は、importFile が入ったときに描かれる（読み込み中の表示の中）。
  useEffect(() => {
    const video = importVideoRef.current;
    if (!importFile || !video) return;
    const controller = new AbortController();
    let active = true;
    setNotice(null);
    framesFromVideo(importFile, {
      video,
      room: roomRef.current,
      signal: controller.signal,
      onProgress: (p) => {
        if (active) setImportProgress(p);
      },
    })
      .then((loaded) => {
        if (!active) return;
        const rejected = onAddRef.current(loaded);
        if (rejected > 0) setNotice(`上限（${limits.maxImages}枚）を超えた${rejected}枚は追加しませんでした。`);
        // 選んだ写真を確かめてもらう（ぼけ・不要な写真を外せる）。
        onReviewRef.current();
      })
      .catch((err: unknown) => {
        if (!active || (err instanceof DOMException && err.name === 'AbortError')) return;
        setNotice(err instanceof Error ? err.message : '動画を読み込めませんでした。');
      })
      .finally(() => {
        if (!active) return;
        setImportFile(null);
        setImportProgress(null);
      });
    return () => {
      active = false;
      controller.abort();
    };
    // limits.maxImages は案内の文言にだけ使う。変わっても読み込みをやり直さない。
  }, [importFile]);

  const cancelImport = useCallback(() => {
    setImportFile(null);
    setImportProgress(null);
  }, []);

  /** 動画を選んだら読み込みを始める。 */
  const pickVideo = useCallback((event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    // 同じ動画をもう一度選べるよう、入力欄を空に戻す。
    event.target.value = '';
    if (file) setImportFile(file);
  }, []);
  const openVideo = () => videoInput.current?.click();

  // 動画で撮っている間は画面を消さない（1周に30秒ほどかかり、その間は画面に触れないため）。
  const wakeLock = useRef<WakeLockSentinel | null>(null);
  const releaseWakeLock = useCallback(() => {
    void wakeLock.current?.release().catch(() => {});
    wakeLock.current = null;
  }, []);

  /** 動画で撮る: 撮影を始める。 */
  const startRecording = () => {
    const video = camera.videoRef.current;
    if (!video || !ready || blocked || full) return;
    setNotice(null);
    let rec: LiveRecording;
    try {
      rec = startLiveRecording(video, {
        room: limits.maxImages - frames.length,
        // 毎コマ描き直さないよう、秒か枚数が変わったときだけ表示を更新する。
        onProgress: (p) =>
          setRecording((prev) =>
            prev && Math.floor(prev.elapsed) === Math.floor(p.elapsed) && prev.picked === p.picked ? prev : p,
          ),
      });
    } catch (err) {
      setNotice(err instanceof Error ? err.message : '撮影を始められませんでした。');
      return;
    }
    recorderRef.current = rec;
    setRecording({ elapsed: 0, picked: 0, intervalSec: 0 });
    navigator.vibrate?.(12);
    (navigator as Partial<Navigator>).wakeLock
      ?.request('screen')
      .then((lock) => {
        wakeLock.current = lock;
      })
      .catch(() => {});
  };

  /** 動画で撮る: 撮影を止め、選んだ写真を一覧に足して確認画面へ進む。 */
  const stopRecording = useCallback(async () => {
    const rec = recorderRef.current;
    if (!rec) return;
    recorderRef.current = null;
    setFinishing(true);
    releaseWakeLock();
    navigator.vibrate?.(12);
    try {
      const loaded = await rec.stop();
      if (loaded.length === 0) {
        setNotice('写真を選べませんでした。対象のまわりを歩きながら、もう少し長く撮ってください。');
        return;
      }
      const rejected = onAddRef.current(loaded);
      if (rejected > 0) setNotice(`上限を超えた${rejected}枚は追加しませんでした。`);
      onReviewRef.current();
    } catch (err) {
      setNotice(err instanceof Error ? err.message : '写真にできませんでした。');
    } finally {
      setRecording(null);
      setFinishing(false);
    }
  }, [releaseWakeLock]);

  // カメラが止まったら（画面を裏に回した等）、そこまでの分で撮影を終える。
  useEffect(() => {
    if (!ready && recorderRef.current) void stopRecording();
  }, [ready, stopRecording]);

  // 画面を離れたら、撮影をやめて写真も捨てる（受け取る一覧が無くなるため）。
  useEffect(
    () => () => {
      recorderRef.current?.cancel();
      recorderRef.current = null;
      releaseWakeLock();
    },
    [releaseWakeLock],
  );

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
            <div className="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/75 to-transparent px-4 pb-3 pt-10 text-center text-sm">
              {auto && !videoMode && !blocked && <AutoMeter advice={advice} progress={progress} />}
              {isRecording && <RecordingBadge progress={recording} />}
              {paused
                ? '写真を送信しています。送信が終わるまで撮影を止めています。'
                : videoMode
                  ? videoHint(isRecording, finishing)
                  : auto
                    ? autoHint(advice, tooFar)
                    : (hint ?? captureHint(frames.length, limits))}
            </div>
          </>
        )}
        {/* 重ねる表示は、カメラが使えないときも出す（写真を選んで送る場合も位置合わせはできる）。 */}
        {overlay && <div className="pointer-events-none absolute inset-x-0 top-0 z-10 p-3">{overlay}</div>}
        {flash && <div className="pointer-events-none absolute inset-0 bg-white/70" />}

        {camera.state.kind === 'idle' && (
          <Overlay>
            <VideoIcon width={36} height={36} className="text-mute" />
            <p className="max-w-xs text-center text-sm text-mute">
              いちばん簡単なのは、対象のまわりを歩きながら動画で1周撮ることです。写真を1枚ずつ撮ることもできます。
            </p>
            <div className="grid w-full max-w-xs grid-cols-2 gap-2">
              <Button
                variant="primary"
                onClick={() => {
                  setVideoMode(true);
                  void camera.start();
                }}
              >
                <VideoIcon />
                動画で撮る
              </Button>
              <Button onClick={openVideo}>
                <VideoIcon />
                動画を選ぶ
              </Button>
              <Button
                onClick={() => {
                  setVideoMode(false);
                  void camera.start();
                }}
              >
                <CameraIcon />
                写真で撮る
              </Button>
              <Button onClick={() => fileInput.current?.click()}>
                <ImagesIcon />
                写真を選ぶ
              </Button>
            </div>
            <p className="text-xs text-mute">撮るにはカメラの使用許可が必要です。</p>
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
            <Button variant="primary" onClick={openVideo}>
              <VideoIcon />
              動画を選ぶ
            </Button>
            <Button onClick={() => fileInput.current?.click()}>
              <ImagesIcon />
              写真を選ぶ
            </Button>
            {/* HTTPS でない・非対応の場合は、やり直しても結果が変わらないのでボタンを出さない。 */}
            {camera.state.failure.problem !== 'insecure' && camera.state.failure.problem !== 'unsupported' && (
              <Button onClick={() => void camera.start()}>もう一度カメラを試す</Button>
            )}
          </Overlay>
        )}

        {/* 動画の読み込み中は、映像の領域でその動画を再生して見せる（どこまで読んだかが分かる）。 */}
        {importing && (
          <div className="absolute inset-0 z-20 flex flex-col bg-black">
            <video ref={importVideoRef} muted playsInline className="min-h-0 w-full flex-1 object-contain" />
            <VideoImportStatus progress={importProgress} onCancel={cancelImport} />
          </div>
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
          <ModeSwitch video={videoMode} onChange={setVideoMode} disabled={isRecording || finishing} />
          {videoMode ? (
            <button
              type="button"
              onClick={openVideo}
              disabled={blocked || full}
              className="flex min-h-9 items-center gap-1.5 rounded-full border border-line px-3 text-sm disabled:text-mute"
            >
              <VideoIcon width={16} height={16} />
              動画を選ぶ
            </button>
          ) : (
            <>
              <Toggle on={auto} onChange={setAuto} label="自動撮影" />
              {camera.state.kind === 'ready' && camera.state.still && (
                <Toggle on={still && !stillBroken.current} onChange={setStill} label="静止画" />
              )}
            </>
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
          {/* 動画は1本ずつ。スマホでは、この場でカメラアプリを開いて撮ることもできる。 */}
          <input ref={videoInput} type="file" accept="video/*" hidden onChange={pickVideo} />
          <button
            type="button"
            className={buttonClass('secondary', 'px-3')}
            onClick={() => fileInput.current?.click()}
            disabled={busy || full || blocked}
          >
            <ImagesIcon />
            <span className="text-sm">追加</span>
          </button>
        </div>

        {videoMode ? (
          // 録画ボタン: 押すと撮り始め、もう一度押すと止める（赤い丸 → 赤い四角）。
          <button
            type="button"
            onClick={() => (isRecording ? void stopRecording() : startRecording())}
            disabled={!ready || finishing || (!isRecording && (busy || full || blocked))}
            aria-label={isRecording ? '撮影を止める' : '動画で撮り始める'}
            className="group grid size-[72px] place-items-center rounded-full border-[3px] border-paper disabled:border-line"
          >
            <span
              className={`bg-bad transition-all group-disabled:bg-line ${
                isRecording ? 'size-[28px] rounded-md' : 'size-[56px] rounded-full group-active:scale-90'
              }`}
            />
          </button>
        ) : (
          <button
            type="button"
            onClick={() => void shoot()}
            disabled={!ready || busy || full || blocked}
            aria-label="撮影する"
            className="group grid size-[72px] place-items-center rounded-full border-[3px] border-paper disabled:border-line"
          >
            <span className="size-[56px] rounded-full bg-paper transition-transform group-active:scale-90 group-disabled:bg-line" />
          </button>
        )}

        <div className="justify-self-end">
          <button
            type="button"
            className={buttonClass(frames.length >= limits.minImages ? 'primary' : 'secondary', 'px-3')}
            onClick={onReview}
            disabled={frames.length === 0 || importing || isRecording || finishing}
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

/** 動画で撮るときの案内。 */
function videoHint(recording: boolean, finishing: boolean): string {
  if (finishing) return '写真にしています…';
  if (recording) return '対象のまわりを、歩きながらゆっくり1周してください。1周したら停止します。';
  return '録画ボタンを押して、対象のまわりを歩きながら1周撮ります（10〜30秒）。';
}

/** 動画で撮っている間の表示（経過時間と、写真にした枚数）。 */
function RecordingBadge({ progress }: { progress: LiveProgress | null }) {
  if (!progress) return null;
  const s = Math.floor(progress.elapsed);
  return (
    <div className="mx-auto mb-2 flex w-fit items-center gap-2 rounded-full bg-black/60 px-3 py-1 text-sm">
      <span className="size-2.5 animate-pulse rounded-full bg-bad" aria-hidden="true" />
      <span className="tabular">
        {Math.floor(s / 60)}:{String(s % 60).padStart(2, '0')}
      </span>
      <span className="tabular text-mute">・{progress.picked}枚</span>
    </div>
  );
}

/** 撮り方の切り替え（動画 / 写真）。 */
function ModeSwitch({
  video,
  onChange,
  disabled,
}: {
  video: boolean;
  onChange: (video: boolean) => void;
  disabled: boolean;
}) {
  const options: [string, boolean][] = [
    ['動画', true],
    ['写真', false],
  ];
  return (
    <div role="radiogroup" aria-label="撮り方" className="flex rounded-full border border-line p-0.5 text-sm">
      {options.map(([label, value]) => (
        <button
          key={label}
          type="button"
          role="radio"
          aria-checked={video === value}
          disabled={disabled}
          onClick={() => onChange(value)}
          className={`min-h-8 rounded-full px-3 ${video === value ? 'bg-amber font-semibold text-ink' : 'text-mute'}`}
        >
          {label}
        </button>
      ))}
    </div>
  );
}

/** 動画の読み込みの進み具合と「中止」。 */
function VideoImportStatus({ progress, onCancel }: { progress: VideoProgress | null; onCancel: () => void }) {
  const percent = progress && progress.duration > 0 ? Math.round((progress.time / progress.duration) * 100) : 0;
  return (
    <div className="space-y-2 border-t border-line bg-panel px-4 py-3">
      <div className="flex items-baseline justify-between text-sm">
        <span>動画から写真を選んでいます</span>
        {progress && (
          <span className="tabular text-mute">
            {progress.picked} / {progress.count}枚
          </span>
        )}
      </div>
      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={percent}
        className="h-1.5 overflow-hidden rounded-full bg-raised"
      >
        <div className="h-full bg-amber transition-[width] duration-200" style={{ width: `${percent}%` }} />
      </div>
      <p className="text-xs text-mute">
        {progress
          ? `動画を最後まで再生しながら、1秒に3枚、ぶれの少ないコマを選びます（約${Math.ceil(progress.duration)}秒）。`
          : '動画を読み込んでいます…'}
      </p>
      <Button className="w-full" onClick={onCancel}>
        中止
      </Button>
    </div>
  );
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
