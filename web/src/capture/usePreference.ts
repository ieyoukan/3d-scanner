// 端末に覚えておく設定（localStorage）。撮り方の選択など、次に開いたときも同じにしたいもの。
import { useCallback, useState } from 'react';

/** 端末に覚えておく設定（次に開いたときも同じにする）。 */
export function usePreference(key: string, initial: boolean): [boolean, (v: boolean) => void] {
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
