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

  /**
   * @param windowAt 時刻（秒）から区間の番号を求める関数
   * @param count    区間の数（終わりが決まっていなければ Infinity。missing は使えない）
   */
  constructor(
    private readonly windowAt: (t: number) => number,
    readonly count = Infinity,
  ) {}

  /** 長さの決まった動画を count 個の同じ長さの区間に分ける。 */
  static forDuration(durationSec: number, count: number): BestPerWindow {
    return new BestPerWindow((t) => windowOf(t, durationSec, count), count);
  }

  /** intervalSec 秒ごとに区切る（終わりの決まっていない、カメラの映像用）。 */
  static every(intervalSec: number): BestPerWindow {
    return new BestPerWindow((t) => Math.max(0, Math.floor(t / intervalSec)));
  }

  offer(t: number, score: number): { window: number; keep: boolean; closed: number[] } {
    const window = this.windowAt(t);
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
    if (!Number.isFinite(this.count)) return [];
    return [...Array(this.count).keys()].filter((w) => !this.done.has(w) && !this.best.has(w));
  }

  private close(match: (w: number) => boolean): number[] {
    const closed = [...this.best.keys()].filter((w) => match(w) && !this.done.has(w)).sort((a, b) => a - b);
    for (const w of closed) this.done.add(w);
    return closed;
  }
}

/**
 * 区間ごとの候補のコマを控え、区間が確定したら順番に写真（JPEG）にする係。
 * 動画ファイル（framesFromVideo）とカメラの映像（liveVideo.ts）の両方で使う。
 *
 * 控えるのは各区間で最新の候補1枚だけ（展開した画素は1枚で約8〜11MB あるので、溜めない）。
 * 写真への変換は1枚ずつ順番に行う。1枚が失敗しても残りは続ける。
 */
export class FrameCollector {
  readonly frames: Frame[] = [];
  /** 変換に失敗した枚数。 */
  failures = 0;
  private readonly candidates = new Map<number, { capturedAt: string; bitmap: Promise<ImageBitmap> }>();
  private encoding: Promise<void> = Promise.resolve();
  private discarded = false;

  /**
   * @param source  写し取る映像
   * @param onFrame 写真が1枚できるたびに呼ばれる
   */
  constructor(
    private readonly source: HTMLVideoElement,
    private readonly onFrame?: (frame: Frame) => void,
  ) {}

  /** いま表示されているコマを、区間 key の候補として写し取る（前の候補は捨てる）。 */
  keep(key: number, capturedAt: string): void {
    this.drop(key);
    this.candidates.set(key, { capturedAt, bitmap: createImageBitmap(this.source) });
  }

  /** 確定した区間の候補を、順番に写真にする。 */
  flush(keys: readonly number[]): void {
    for (const key of keys) {
      const c = this.candidates.get(key);
      if (!c) continue;
      this.candidates.delete(key);
      this.encoding = this.encoding.then(async () => {
        let bitmap: ImageBitmap | undefined;
        try {
          bitmap = await c.bitmap;
          if (this.discarded) return;
          const frame = await frameFromVideoFrame(bitmap, c.capturedAt);
          if (this.discarded) {
            releaseFrame(frame);
            return;
          }
          this.frames.push(frame);
          this.onFrame?.(frame);
        } catch {
          this.failures += 1;
        } finally {
          bitmap?.close();
        }
      });
    }
  }

  /** 写真の一覧を入れ替える（間引いたとき）。外した写真の解放は呼び出し側が行う。 */
  replace(frames: readonly Frame[]): void {
    this.frames.splice(0, this.frames.length, ...frames);
  }

  /** ここまでに頼んだ変換が終わるのを待つ。 */
  settled(): Promise<void> {
    return this.encoding;
  }

  /** 撮影順に並べた写真の一覧（capturedAt は同じ書式の ISO 文字列なので、文字列の順が時刻の順）。 */
  sorted(): Frame[] {
    return [...this.frames].sort((a, b) => a.capturedAt.localeCompare(b.capturedAt));
  }

  /** 控えている候補を捨てる。 */
  dropCandidates(): void {
    for (const key of [...this.candidates.keys()]) this.drop(key);
  }

  /** 中止したとき: 候補も、作った写真もすべて捨てる。 */
  async discard(): Promise<void> {
    this.discarded = true;
    this.dropCandidates();
    await this.encoding;
    for (const f of this.frames) releaseFrame(f);
    this.frames.length = 0;
  }

  private drop(key: number): void {
    const c = this.candidates.get(key);
    if (!c) return;
    this.candidates.delete(key);
    c.bitmap.then((b) => b.close()).catch(() => {});
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
  const collector = new FrameCollector(video);
  const cleanups: (() => void)[] = [];
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

    const picker = BestPerWindow.forDuration(duration, count);
    const scorer = makeScorer(video);
    // 動画の撮影開始の時刻（並び順の手掛かり）。ファイルの更新時刻を撮影終了とみなす。
    const startedAt = (file.lastModified || Date.now()) - duration * 1000;
    const report = (time: number) =>
      opts.onProgress({ time, duration, picked: collector.frames.length, count });

    /** いまのコマが区間で一番なら写し取って控え、確定した区間を写真にする。 */
    const consider = async (t: number) => {
      const score = await scorer();
      const r = picker.offer(t, score);
      if (r.keep) collector.keep(r.window, new Date(startedAt + t * 1000).toISOString());
      collector.flush(r.closed);
    };
    const gate = oneAtATime();
    const flush = (windows: number[]) => collector.flush(windows);

    report(0);
    // 古いブラウザには requestVideoFrameCallback が無い（型の上では常にあるので、関数かどうかで確かめる）。
    if (typeof (video as Partial<HTMLVideoElement>).requestVideoFrameCallback === 'function') {
      await playThrough(video, signal, cleanups, (t) => {
        gate.run(() => consider(t));
        report(t);
      });
      await gate.idle();
    } else {
      for (let w = 0; w < count; w++) {
        for (let k = 0; k < seekSamples; k++) {
          const t = Math.min(duration - 0.001, ((w + (k + 0.5) / seekSamples) * duration) / count);
          video.currentTime = t;
          await waitFor(video, 'seeked', signal, cleanups);
          await consider(t);
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
      await consider(t);
      flush(picker.finish());
    }
    await collector.settled();
    if (signal.aborted) throw abortError();
    report(duration);
    if (collector.frames.length === 0) throw new FrameError('動画からコマを読み取れませんでした。');
    // 後から埋めた区間の分も含めて、動画の中の順に並べる。
    return collector.sorted();
  } catch (err) {
    // 途中で終わったら、変換中の分も含めて、作った写真を捨てる。
    await collector.discard();
    if (signal.aborted) throw abortError();
    if (err instanceof FrameError || (err instanceof DOMException && err.name === 'AbortError')) throw err;
    throw new FrameError(
      `「${file.name}」を動画として読み込めませんでした。このブラウザで再生できない形式かもしれません。`,
    );
  } finally {
    collector.dropCandidates();
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

/**
 * いま表示されているコマのくっきり度を測る関数を作る（長辺 480px に縮めたラプラシアン分散）。
 *
 * 縮小は GPU に任せる（createImageBitmap の resizeWidth / resizeHeight）。読み出し用の canvas に
 * フル解像度の映像を直接描くと、毎コマ映像全体を GPU から読み出してから縮めることになり、
 * 1080×1920 の動画を再生しながら測ると1コマ平均 7.6ms かかった（GPU で縮めると 3.1ms。Mac の Chrome）。
 * 縮小の指定に対応していないブラウザでは、canvas に直接描く。
 *
 * 測り終わるのは、コマが表示されてから数ミリ秒後。次のコマ（30fps なら 33ms 後）より前なので、
 * 呼び出し側が測り終えてから createImageBitmap で写し取っても、ほぼ同じコマが取れる。
 */
export function makeScorer(video: HTMLVideoElement): () => Promise<number> {
  const size = fit(video.videoWidth, video.videoHeight, scoreEdge);
  const canvas = document.createElement('canvas');
  canvas.width = size.width;
  canvas.height = size.height;
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  if (!ctx) throw new FrameError('画像を処理できませんでした。');
  const gray = new Float32Array(size.width * size.height);
  let gpuResize = true;
  const measure = () => {
    const pixels = ctx.getImageData(0, 0, size.width, size.height);
    return laplacianVariance(toGray(pixels.data, gray), size.width, size.height);
  };
  return async () => {
    if (gpuResize) {
      try {
        const small = await createImageBitmap(video, {
          resizeWidth: size.width,
          resizeHeight: size.height,
          resizeQuality: 'low',
        });
        ctx.drawImage(small, 0, 0);
        small.close();
        return measure();
      } catch {
        gpuResize = false;
      }
    }
    ctx.drawImage(video, 0, 0, size.width, size.height);
    return measure();
  };
}

/**
 * 測っている最中に次のコマが来たら、そのコマは飛ばす係。遅い端末では、自動的に測る枚数が減る
 * （画面の更新が詰まって撮影や操作がカクつくより、候補が少し減る方がよい）。
 */
export function oneAtATime(): { run: (task: () => Promise<void>) => void; idle: () => Promise<void> } {
  let current: Promise<void> | null = null;
  return {
    run(task) {
      if (current) return;
      current = task()
        .catch(() => {})
        .finally(() => {
          current = null;
        });
    },
    idle: () => current ?? Promise.resolve(),
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
