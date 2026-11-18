// その場で動画を撮る: カメラの映像を見ながら、1/3 秒ごとの区間で一番くっきりしたコマを写真にしていく。
//
// 動画ファイルから選ぶ（video.ts）のと同じ選び方を、録画せずに撮影中に行う。
//   - 動画として圧縮する前のカメラの映像から取るので、画質が落ちない。
//   - 停止した時点で写真がそろっている（後から動画を再生し直す待ち時間が無い）。
//
// 長く撮ったとき: 写真が上限（追加できる残りの枚数）に達したら、隣り合う2枚ずつ組にして
// くっきりした方を残し（約半分になる）、以後は区間の長さを倍にする。何秒撮っても上限に収まり、
// 1周の中にまんべんなく写真が残る。
import { releaseFrame, type Frame } from './frames';
import { BestPerWindow, FrameCollector, framesPerSecond, makeScorer } from './video';

/**
 * 隣り合う2枚ずつ組にして、くっきり度（sharpness）の高い方を残す。並び順は保つ。
 * 奇数枚なら最後の1枚はそのまま残す。
 */
export function thinPairs<T extends { sharpness: number }>(items: readonly T[]): { kept: T[]; dropped: T[] } {
  const kept: T[] = [];
  const dropped: T[] = [];
  for (let i = 0; i < items.length; i += 2) {
    const a = items[i]!;
    const b = items[i + 1];
    if (b === undefined) {
      kept.push(a);
    } else if (b.sharpness > a.sharpness) {
      kept.push(b);
      dropped.push(a);
    } else {
      kept.push(a);
      dropped.push(b);
    }
  }
  return { kept, dropped };
}

export interface LiveProgress {
  /** 撮り始めてからの秒数。 */
  elapsed: number;
  /** 写真にした枚数。 */
  picked: number;
  /** いまの区間の長さ（秒）。上限に達して間引くたびに倍になる。 */
  intervalSec: number;
}

export interface LiveRecording {
  /** 撮影を止めて、選んだ写真を撮影順に返す。 */
  stop(): Promise<Frame[]>;
  /** 撮影をやめて、選んだ写真も捨てる（画面を離れたときなど）。 */
  cancel(): void;
}

/**
 * カメラの映像（再生中の <video>）から、写真を選び始める。
 * @param room 追加できる残りの枚数（これを超えないよう間引く）
 */
export function startLiveRecording(
  video: HTMLVideoElement,
  opts: { room: number; onProgress: (p: LiveProgress) => void },
): LiveRecording {
  const room = Math.max(1, opts.room);
  const scorer = makeScorer(video);
  const t0 = performance.now();
  let running = true;
  let elapsed = 0;

  // 区間は「段」に分けて数える。間引くたびに段を進め、区間の長さを倍にする。
  // 区間の番号は段ごとに 0 から数え直すので、控えの鍵は段と番号を組み合わせて重ならないようにする。
  let stage = 0;
  let stageStart = 0;
  let intervalSec = 1 / framesPerSecond;
  let picker = BestPerWindow.every(intervalSec);
  const key = (window: number) => stage * 1_000_000 + window;

  const report = () => opts.onProgress({ elapsed, picked: collector.frames.length, intervalSec });

  const collector = new FrameCollector(video, () => {
    if (collector.frames.length >= room && room >= 2) {
      // 上限に達した。2枚ずつ組にしてくっきりした方を残し、以後は区間を倍にする。
      const { kept, dropped } = thinPairs(collector.sorted());
      dropped.forEach(releaseFrame);
      collector.replace(kept);
      collector.flush(picker.finish().map(key));
      stage += 1;
      stageStart = elapsed;
      intervalSec *= 2;
      picker = BestPerWindow.every(intervalSec);
    }
    report();
  });

  const tick = (now: number) => {
    if (!running) return;
    elapsed = (now - t0) / 1000;
    // カメラが止まっている（裏に回った等）ときは、映像が無いので見ない。
    if (video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA && video.videoWidth > 0) {
      const r = picker.offer(elapsed - stageStart, scorer());
      if (r.keep) collector.keep(key(r.window), new Date().toISOString());
      collector.flush(r.closed.map(key));
    }
    report();
    schedule();
  };

  // 新しいコマが届くたびに見る。requestVideoFrameCallback が無いブラウザでは画面の描画ごとに見る。
  const hasFrameCallback = typeof (video as Partial<HTMLVideoElement>).requestVideoFrameCallback === 'function';
  const schedule = () => {
    if (hasFrameCallback) video.requestVideoFrameCallback((now) => tick(now));
    else requestAnimationFrame(tick);
  };
  schedule();

  return {
    async stop() {
      running = false;
      collector.flush(picker.finish().map(key));
      await collector.settled();
      // 止めた時点で候補が残っていて上限を超えたら、上限に収まるまで間引く。
      let frames = collector.sorted();
      while (frames.length > room && room >= 2) {
        const { kept, dropped } = thinPairs(frames);
        dropped.forEach(releaseFrame);
        frames = kept;
      }
      collector.replace(frames);
      return frames;
    },
    cancel() {
      running = false;
      void collector.discard();
    },
  };
}
