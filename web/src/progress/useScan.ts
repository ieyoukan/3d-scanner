// スキャンの状態をサーバーから定期的に取得する React フック（ポーリング）。
//
// 流れ:
//   マウント → すぐ1回取得 → 終了状態でなければ 2 秒後にもう一度 → … → 終了状態になったら止める
//
// WebSocket などの双方向通信を使わない理由:
//   状態が変わるのは数十秒〜数分に1回で、2秒おきの問い合わせで十分追える。
//   ポーリングは普通の HTTP なので、途中の中継（Cloudflare）や再接続の心配が要らず、仕組みも単純。
import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, fetchScan } from '../api/client';
import { isTerminal, type Scan } from '../api/types';

const pollIntervalMs = 2000;

export type ScanLoad =
  | { kind: 'loading' }
  | { kind: 'ready'; scan: Scan; stale: boolean }
  | { kind: 'not-found' }
  | { kind: 'error'; message: string };

export function useScan(id: string) {
  const [load, setLoad] = useState<ScanLoad>({ kind: 'loading' });
  // refresh() が呼ばれるたびに増やす番号。useEffect の依存に入れて、取得をやり直させる。
  const [generation, setGeneration] = useState(0);
  // 直近に取得できたスキャン。通信が一時的に失敗しても、画面には最後の状態を出し続ける。
  const last = useRef<Scan | null>(null);

  useEffect(() => {
    // この effect の実行1回ぶんの「取り消しスイッチ」。
    // id が変わる・画面を離れるときに、進行中の通信と次回の予約をまとめて止める。
    const controller = new AbortController();
    let timer: number | undefined;

    const tick = async () => {
      try {
        const scan = await fetchScan(id, controller.signal);
        last.current = scan;
        setLoad({ kind: 'ready', scan, stale: false });
        if (isTerminal(scan.status)) return; // 終了。もう問い合わせない
      } catch (err) {
        if (controller.signal.aborted) return;
        if (err instanceof ApiError && err.status === 404) {
          setLoad({ kind: 'not-found' });
          return;
        }
        // 一時的な通信エラー。最後の状態があればそれを「古いかもしれない」印つきで出し、再試行を続ける。
        if (last.current) {
          setLoad({ kind: 'ready', scan: last.current, stale: true });
        } else {
          setLoad({ kind: 'error', message: err instanceof Error ? err.message : '通信に失敗しました。' });
        }
      }
      // タブが裏にある間は問い合わせない（電池と通信量の節約）。表に戻ったら onVisible が再開する。
      if (!document.hidden) timer = window.setTimeout(() => void tick(), pollIntervalMs);
    };

    const onVisible = () => {
      if (!document.hidden) {
        window.clearTimeout(timer);
        void tick();
      }
    };
    document.addEventListener('visibilitychange', onVisible);
    void tick();

    // 後片付け。これを書かないと、画面を離れた後も通信が続き、
    // 既に無い部品の state を更新しようとしてしまう。
    return () => {
      controller.abort();
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [id, generation]);

  /** いますぐ取り直す（操作の直後など）。 */
  const refresh = useCallback(() => setGeneration((g) => g + 1), []);

  return { load, refresh };
}

/** 1秒ごとに現在時刻を返す。active が false の間は止まる（経過時間の表示用）。 */
export function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [active]);
  return now;
}
