// 撮影・選択した写真の一覧を持つ React フック。
//
// 写真はブラウザのメモリ上にだけある（ページを再読み込みすると消える）。
// MVP では「撮ってすぐ送る」流れなので保存はしない。そのかわり、写真が残っている間は
// ページを離れる前にブラウザの確認ダイアログを出す。
import { useCallback, useEffect, useRef, useState } from 'react';
import { releaseFrame, type Frame } from './frames';

export function useFrames(maxImages: number) {
  const [frames, setFrames] = useState<Frame[]>([]);
  // 最新の一覧を、後片付け（アンマウント時）から参照するための控え。
  const latest = useRef<Frame[]>([]);
  latest.current = frames;

  /** 写真を末尾に追加する。上限を超えるぶんは受け取らず、その枚数を返す。 */
  const add = useCallback(
    (incoming: Frame[]): number => {
      const room = Math.max(0, maxImages - latest.current.length);
      const accepted = incoming.slice(0, room);
      const rejected = incoming.slice(room);
      rejected.forEach(releaseFrame);
      if (accepted.length > 0) {
        // 更新関数の形（prev => ...）で書くと、連続して呼ばれても取りこぼさない。
        setFrames((prev) => [...prev, ...accepted]);
      }
      return rejected.length;
    },
    [maxImages],
  );

  // URL の解放（副作用）は更新関数の外で行う。React は開発時に更新関数を2回呼んで
  // 「副作用が紛れ込んでいないか」を確かめるので、更新関数の中は計算だけにしておく。
  const remove = useCallback((id: string) => {
    const target = latest.current.find((f) => f.id === id);
    if (target) releaseFrame(target);
    setFrames((prev) => prev.filter((f) => f.id !== id));
  }, []);

  const clear = useCallback(() => {
    latest.current.forEach(releaseFrame);
    setFrames([]);
  }, []);

  // 画面を離れるとき、残っているサムネイルの URL を解放する。
  useEffect(() => () => latest.current.forEach(releaseFrame), []);

  // 写真が残っているのにページを閉じよう・再読み込みしようとしたら確認を出す。
  useEffect(() => {
    if (frames.length === 0) return;
    const warn = (event: BeforeUnloadEvent) => event.preventDefault();
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [frames.length]);

  return { frames, add, remove, clear };
}
