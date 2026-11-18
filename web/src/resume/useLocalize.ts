// 位置合わせ（VPS）の状態を持つ React フック。
//
// 写真1枚をサーバーに送ると、前回の3Dデータ（地図）のどこから・どちらを向いて撮ったかが返る。
// サーバーで COLMAP を動かすので 10〜数十秒かかる。同時に1件だけ問い合わせ、新しい問い合わせを
// 始めたら前のものは取り消す（結果を待っている間に撮った、より新しい写真を優先する）。
//
// 位置が分かった写真は fixes に貯める。地図に「どこで撮ったか」を点で残し、撮影範囲の広がりを見せる。
import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, localizeFrame } from '../api/client';
import type { LocalizeResult } from '../api/types';
import type { Frame } from '../capture/frames';
import type { Vec3 } from './mapView';

/** 位置が分かった写真1枚。 */
export interface Fix {
  frameId: string;
  position: Vec3;
  forward: Vec3;
  matches: number;
}

export type LocalizeState =
  | { kind: 'idle' }
  | { kind: 'checking'; frameId: string }
  | { kind: 'found'; frameId: string; result: LocalizeResult }
  | { kind: 'not-found'; frameId: string; result: LocalizeResult }
  | { kind: 'error'; frameId: string; message: string };

export function useLocalize(scanId: string) {
  const [state, setState] = useState<LocalizeState>({ kind: 'idle' });
  const [fixes, setFixes] = useState<Fix[]>([]);
  const inflight = useRef<AbortController | null>(null);

  const check = useCallback(
    async (frame: Frame) => {
      inflight.current?.abort();
      const controller = new AbortController();
      inflight.current = controller;
      setState({ kind: 'checking', frameId: frame.id });
      try {
        const result = await localizeFrame(scanId, frame.blob, controller.signal);
        if (controller.signal.aborted) return;
        if (result.localized && result.position && result.forward) {
          const fix: Fix = { frameId: frame.id, position: result.position, forward: result.forward, matches: result.matches };
          setFixes((prev) => [...prev.filter((f) => f.frameId !== frame.id), fix]);
          setState({ kind: 'found', frameId: frame.id, result });
        } else {
          setState({ kind: 'not-found', frameId: frame.id, result });
        }
      } catch (err) {
        if (controller.signal.aborted) return;
        const message =
          err instanceof ApiError || err instanceof Error ? err.message : '位置を確かめられませんでした。';
        setState({ kind: 'error', frameId: frame.id, message });
      } finally {
        if (inflight.current === controller) inflight.current = null;
      }
    },
    [scanId],
  );

  /** 写真を削除したら、その写真の位置も地図から消す。確認中ならやめる。 */
  const forget = useCallback((frameId: string) => {
    setFixes((prev) => prev.filter((f) => f.frameId !== frameId));
    setState((prev) => {
      if (prev.kind === 'idle' || prev.frameId !== frameId) return prev;
      if (prev.kind === 'checking') inflight.current?.abort();
      return { kind: 'idle' };
    });
  }, []);

  /** 写真をすべて削除したときに、位置の記録も消す。 */
  const reset = useCallback(() => {
    inflight.current?.abort();
    setFixes([]);
    setState({ kind: 'idle' });
  }, []);

  // 画面を離れたら問い合わせをやめる（サーバー側の処理も、接続が切れると止まる）。
  useEffect(() => () => inflight.current?.abort(), []);

  return { state, fixes, check, forget, reset };
}
