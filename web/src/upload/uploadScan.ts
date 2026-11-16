// 写真をサーバーへ送る（POST /api/scans）。
//
// 送る形式は multipart/form-data:
//   images    写真のファイル（枚数ぶん繰り返す）
//   metadata  撮影時刻などの付加情報（JSON 文字列）
//
// fetch ではなく XMLHttpRequest を使う理由:
//   送信の進み具合（何バイト送れたか）を知る手段が、XHR の upload.onprogress しか
//   広く使えないため。数十MBをモバイル回線で送るので、進捗が見えないと不安になる。
//   ここで表示する % は「実際に送れたバイト数 ÷ 全体」であり、作り物の数字ではない。
import { toApiError } from '../api/client';
import type { CreateScanResult } from '../api/types';
import type { Frame } from '../capture/frames';

export interface UploadProgress {
  sentBytes: number;
  totalBytes: number;
}

export interface UploadHandle {
  /** 完了すると、作られたスキャンの情報で解決する。 */
  done: Promise<CreateScanResult>;
  /** 送信を中止する。 */
  abort: () => void;
}

/** 写真と一緒に送る付加情報を組み立てる。 */
export function buildMetadata(frames: readonly Frame[], client: Record<string, unknown>): string {
  return JSON.stringify({
    client,
    // frames は images と同じ順番で並べる（サーバーは順番で対応づける）。
    frames: frames.map((f) => ({
      capturedAt: f.capturedAt,
      source: f.source,
      width: f.width,
      height: f.height,
    })),
  });
}

/** 端末・ブラウザの情報。将来、端末ごとのカメラ特性を扱うときの手掛かりになる。 */
function clientInfo(): Record<string, unknown> {
  return {
    userAgent: navigator.userAgent,
    language: navigator.language,
    screen: { width: window.screen.width, height: window.screen.height, pixelRatio: window.devicePixelRatio },
  };
}

export function uploadScan(frames: readonly Frame[], onProgress: (p: UploadProgress) => void): UploadHandle {
  const form = new FormData();
  frames.forEach((frame, i) => {
    const ext = frame.blob.type === 'image/png' ? 'png' : 'jpg';
    // 第3引数のファイル名は形式上必要なだけ。サーバーは使わず、自分で名前を付け直す。
    form.append('images', frame.blob, `frame-${i + 1}.${ext}`);
  });
  form.append('metadata', buildMetadata(frames, clientInfo()));

  const xhr = new XMLHttpRequest();
  const done = new Promise<CreateScanResult>((resolve, reject) => {
    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress({ sentBytes: event.loaded, totalBytes: event.total });
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          resolve(JSON.parse(xhr.responseText) as CreateScanResult);
        } catch {
          reject(new Error('サーバーの応答を読み取れませんでした。'));
        }
      } else {
        reject(toApiError(xhr.status, xhr.responseText));
      }
    };
    xhr.onerror = () => reject(new Error('通信が途切れました。電波の良い場所でもう一度お試しください。'));
    xhr.onabort = () => reject(new DOMException('アップロードを中止しました。', 'AbortError'));
    xhr.open('POST', '/api/scans');
    // Content-Type は自分で設定しない。FormData を渡すと、ブラウザが区切り文字（boundary）
    // つきの正しいヘッダを自動で付ける。
    xhr.send(form);
  });
  return { done, abort: () => xhr.abort() };
}
