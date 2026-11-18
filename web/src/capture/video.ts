// 動画から写真を選ぶ。対象のまわりを歩きながら1周撮った動画を、写真の一覧（Frame）に変える。
//
// なぜ動画か:
//   歩きながら撮ると、止まるのを待つ自動撮影ではほとんど撮れない。一方、その歩き方の動画から
//   写真を選ぶと、1周の位置合わせがきれいにつながる（10〜13 秒の1周動画3本で、全部の写真の位置を推定でき、
//   最初と最後の写真もつながった。再投影誤差 0.41〜0.50px）。
//
// 選び方: 動画を同じ長さの区間に分け、区間ごとに一番くっきりしたコマを1枚選ぶ。
//   歩きながらの動画には、手ぶれでぼけたコマが混ざる（同じ動画の中でくっきり度が4〜5倍違った）。
//   一定間隔で選ぶより一番くっきりしたコマを選ぶ方が、3D点が27〜33%多く、誤差も約1割小さかった。
//   区間の数は「1秒に3枚」。1秒に2枚だと1周のうち最大59°抜けることがあり、3枚なら32°以内だった。
//
// 読み方:
//   requestVideoFrameCallback が使えるブラウザ: 動画を等倍で再生し、表示されるコマを1枚ずつ測る。
//     動画の長さだけ時間がかかるが、全部のコマを見られる。
//   使えないブラウザ: 区間ごとに3か所へ移動（シーク）して測る。
//   再生中に画面の描画が止まる（通知・一時的な負荷など）と、1枚もコマが届かない区間ができる。
//   そういう区間は、再生の後でその区間の中央へ移動して1枚取る（1周の中に抜けを作らない）。
//
// 選んだコマはその場で JPEG にする。全部の区間のコマを展開したまま持つと、スマホのメモリが足りなくなるため。
import { FrameError, fit, frameFromVideoFrame, releaseFrame, type Frame } from './frames';
import { laplacianVariance, toGray } from './sharpness';

/** 1秒あたりに選ぶ写真の枚数。 */
export const framesPerSecond = 3;
/** これより短い動画は受け付けない（秒）。 */
export const minVideoSeconds = 2;
/** くっきり度を測るときの長辺（画素）。評価に使った値と同じ。 */
const scoreEdge = 480;
/** シークで読むとき、1区間で比べるコマの数。 */
const seekSamples = 3;

/** 動画の長さと、追加できる残りの枚数から、選ぶ枚数を決める。 */
export function planCount(durationSec: number, room: number, perSecond = framesPerSecond): number {
  if (!Number.isFinite(durationSec) || durationSec <= 0 || room < 1) return 0;
  return Math.max(1, Math.min(room, Math.round(durationSec * perSecond)));
}

/** 時刻 t（秒）が何番目の区間か。 */
export function windowOf(t: number, durationSec: number, count: number): number {
  return Math.min(count - 1, Math.max(0, Math.floor((t / durationSec) * count)));
}

/**
 * 区間ごとに、一番くっきりしたコマを覚えておく係（画面に依存しない）。
 *
 * offer にコマを時刻順に見せると、
 *   keep   … そのコマが、いまの区間で一番くっきりしている（写し取って控えるべき）
 *   closed … 先へ進んだので、もう候補が変わらない区間（控えたコマを写真にしてよい）
 * を返す。終わったら finish で残りの区間を確定する。
 */
export class BestPerWindow {
  private readonly best = new Map<number, number>();
  private readonly done = new Set<number>();

  constructor(
    readonly durationSec: number,
    readonly count: number,
  ) {}

  offer(t: number, score: number): { window: number; keep: boolean; closed: number[] } {
    const window = windowOf(t, this.durationSec, this.count);
    const closed = this.close((w) => w < window);
    // 確定済みの区間に戻ってきたコマ（巻き戻りなど）は使わない。
    if (this.done.has(window)) return { window, keep: false, closed };
    const prev = this.best.get(window);
    const keep = prev === undefined || score > prev;
    if (keep) this.best.set(window, score);
    return { window, keep, closed };
  }

  finish(): number[] {
    return this.close(() => true);
  }

  /** まだ1枚もコマを見ていない区間（確定していない区間）。 */
  missing(): number[] {
    return [...Array(this.count).keys()].filter((w) => !this.done.has(w) && !this.best.has(w));
  }

  private close(match: (w: number) => boolean): number[] {
    const closed = [...this.best.keys()].filter((w) => match(w) && !this.done.has(w)).sort((a, b) => a - b);
    for (const w of closed) this.done.add(w);
    return closed;
  }
}

export interface VideoProgress {
  /** 読み終えた位置（秒）。 */
  time: number;
  duration: number;
  /** 写真にした枚数。 */
  picked: number;
  /** 選ぶ予定の枚数。 */
  count: number;
}

export interface VideoOptions {
  /** 再生に使う要素。画面に出しておくと、読んでいる様子が見える。 */
  video: HTMLVideoElement;
  /** 追加できる残りの枚数。 */
  room: number;
  signal: AbortSignal;
  onProgress: (p: VideoProgress) => void;
}

/** 動画ファイルから写真を選ぶ。中止したら AbortError（DOMException）で終わる。 */
export async function framesFromVideo(file: File, opts: VideoOptions): Promise<Frame[]> {
  const { video, signal } = opts;
  const url = URL.createObjectURL(file);
  const candidates = new Map<number, { t: number; bitmap: Promise<ImageBitmap> }>();
  const frames: Frame[] = [];
  const cleanups: (() => void)[] = [];
  // 写真への変換（JPEG 化）を順番に行うための鎖。途中で終わっても、これを待ってから後片付けする。
  let encoding = Promise.resolve();
  try {
    video.muted = true;
    video.playsInline = true;
    video.preload = 'auto';
    video.src = url;
    await waitFor(video, 'loadedmetadata', signal, cleanups);
    const duration = video.duration;
    if (!Number.isFinite(duration) || duration <= 0) {
      throw new FrameError('動画の長さが分かりませんでした。別の動画でお試しください。');
    }
    if (duration < minVideoSeconds) {
      throw new FrameError('動画が短すぎます。対象のまわりを歩きながら、10秒以上かけて1周撮ってください。');
    }
    const count = planCount(duration, opts.room);
    if (count < 1) throw new FrameError('これ以上写真を追加できません。');

    const picker = new BestPerWindow(duration, count);
    const scorer = makeScorer(video);
    // 動画の撮影開始の時刻（並び順の手掛かり）。ファイルの更新時刻を撮影終了とみなす。
    const startedAt = (file.lastModified || Date.now()) - duration * 1000;
    const report = (time: number) => opts.onProgress({ time, duration, picked: frames.length, count });

    /** いまのコマが区間で一番なら写し取って控える。古い候補は閉じる。 */
    const consider = (t: number) => {
      const r = picker.offer(t, scorer());
      if (r.keep) {
        const old = candidates.get(r.window);
        old?.bitmap.then((b) => b.close()).catch(() => {});
        candidates.set(r.window, { t, bitmap: createImageBitmap(video) });
      }
      flush(r.closed);
    };
    /** 確定した区間の候補を、順番に JPEG にする。 */
    const flush = (windows: number[]) => {
      for (const w of windows) {
        const c = candidates.get(w);
        if (!c) continue;
        candidates.delete(w);
        encoding = encoding.then(async () => {
          const bitmap = await c.bitmap;
          try {
            if (signal.aborted) return;
            frames.push(await frameFromVideoFrame(bitmap, new Date(startedAt + c.t * 1000).toISOString()));
          } finally {
            bitmap.close();
          }
        });
      }
    };

    report(0);
    // 古いブラウザには requestVideoFrameCallback が無い（型の上では常にあるので、関数かどうかで確かめる）。
    if (typeof (video as Partial<HTMLVideoElement>).requestVideoFrameCallback === 'function') {
      await playThrough(video, signal, cleanups, (t) => {
        consider(t);
        report(t);
      });
    } else {
      for (let w = 0; w < count; w++) {
        for (let k = 0; k < seekSamples; k++) {
          const t = Math.min(duration - 0.001, ((w + (k + 0.5) / seekSamples) * duration) / count);
          video.currentTime = t;
          await waitFor(video, 'seeked', signal, cleanups);
          consider(t);
        }
        report(((w + 1) * duration) / count);
      }
    }
    flush(picker.finish());
    // コマが届かなかった区間を、移動して埋める。
    for (const w of picker.missing()) {
      const t = Math.min(duration - 0.001, ((w + 0.5) * duration) / count);
      video.pause();
      video.currentTime = t;
      await waitFor(video, 'seeked', signal, cleanups);
      consider(t);
      flush(picker.finish());
    }
    await encoding;
    if (signal.aborted) throw abortError();
    report(duration);
    if (frames.length === 0) throw new FrameError('動画からコマを読み取れませんでした。');
    // 後から埋めた区間の分も含めて、動画の中の順に並べる（capturedAt は同じ書式の ISO 文字列）。
    return frames.sort((a, b) => a.capturedAt.localeCompare(b.capturedAt));
  } catch (err) {
    // 途中で終わったら、変換中の分も含めて、作った写真を捨てる。
    await encoding.catch(() => {});
    for (const f of frames) releaseFrame(f);
    if (signal.aborted) throw abortError();
    if (err instanceof FrameError || (err instanceof DOMException && err.name === 'AbortError')) throw err;
    throw new FrameError(
      `「${file.name}」を動画として読み込めませんでした。このブラウザで再生できない形式かもしれません。`,
    );
  } finally {
    for (const c of candidates.values()) c.bitmap.then((b) => b.close()).catch(() => {});
    for (const fn of cleanups) fn();
    // 同じ要素で次の動画を読み始めていたら（開発時の StrictMode の再実行など）、そちらを壊さない。
    if (video.src === url) {
      video.pause();
      video.removeAttribute('src');
      video.load();
    }
    URL.revokeObjectURL(url);
  }
}

/** いま表示されているコマのくっきり度を測る関数を作る。 */
function makeScorer(video: HTMLVideoElement): () => number {
  const size = fit(video.videoWidth, video.videoHeight, scoreEdge);
  const canvas = document.createElement('canvas');
  canvas.width = size.width;
  canvas.height = size.height;
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  if (!ctx) throw new FrameError('画像を処理できませんでした。');
  return () => {
    ctx.drawImage(video, 0, 0, size.width, size.height);
    const pixels = ctx.getImageData(0, 0, size.width, size.height);
    return laplacianVariance(toGray(pixels.data), size.width, size.height);
  };
}

/**
 * 動画を最後まで等倍で再生し、表示されたコマごとに onFrame(再生位置) を呼ぶ。
 * 画面が裏に回ると再生が止まることがあるので、戻ってきたら再開する。
 */
function playThrough(
  video: HTMLVideoElement,
  signal: AbortSignal,
  cleanups: (() => void)[],
  onFrame: (t: number) => void,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    let finished = false;
    const finish = (err?: unknown) => {
      if (finished) return;
      finished = true;
      if (err === undefined) resolve();
      else reject(err);
    };
    const tick = (_now: number, meta: VideoFrameCallbackMetadata) => {
      if (finished) return;
      try {
        onFrame(meta.mediaTime);
      } catch (err) {
        finish(err);
        return;
      }
      video.requestVideoFrameCallback(tick);
    };
    const onEnded = () => finish();
    const onError = () => finish(new Error('video error'));
    const onAbort = () => finish(abortError());
    const onVisible = () => {
      if (!document.hidden && video.paused && !video.ended) void video.play().catch(() => {});
    };
    video.addEventListener('ended', onEnded);
    video.addEventListener('error', onError);
    signal.addEventListener('abort', onAbort);
    document.addEventListener('visibilitychange', onVisible);
    cleanups.push(() => {
      video.removeEventListener('ended', onEnded);
      video.removeEventListener('error', onError);
      signal.removeEventListener('abort', onAbort);
      document.removeEventListener('visibilitychange', onVisible);
    });
    if (signal.aborted) {
      onAbort();
      return;
    }
    video.requestVideoFrameCallback(tick);
    video.currentTime = 0;
    video.play().catch((err: unknown) => finish(err));
  });
}

/** 要素のイベントを1回待つ。エラー・中止なら失敗にする。 */
function waitFor(el: HTMLVideoElement, type: string, signal: AbortSignal, cleanups: (() => void)[]): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal.aborted) {
      reject(abortError());
      return;
    }
    const off = () => {
      el.removeEventListener(type, onOk);
      el.removeEventListener('error', onErr);
      signal.removeEventListener('abort', onAbort);
    };
    const onOk = () => {
      off();
      resolve();
    };
    const onErr = () => {
      off();
      reject(new Error('video error'));
    };
    const onAbort = () => {
      off();
      reject(abortError());
    };
    el.addEventListener(type, onOk);
    el.addEventListener('error', onErr);
    signal.addEventListener('abort', onAbort);
    cleanups.push(off);
  });
}

function abortError(): DOMException {
  return new DOMException('中止しました', 'AbortError');
}
