// ステップの処理ログ（外部CLIの出力）を、開いたときだけ読み込んで表示する。
//
// 普段は畳んでおく。ログは開発者や学習者には有用だが、ほとんどの利用者には
// 「英語の長い文字列」でしかなく、常に出ていると肝心の状況が埋もれてしまう。
import { useEffect, useState } from 'react';
import { fetchLog } from '../api/client';
import type { StepName } from '../api/types';

interface Props {
  scanId: string;
  step: StepName;
  /** 実行中なら、開いている間は定期的に読み直す。 */
  live: boolean;
}

export function LogViewer({ scanId, step, live }: Props) {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    // 畳んでいる間は通信しない。
    if (!open) return;
    const controller = new AbortController();
    let timer: number | undefined;
    const load = async () => {
      try {
        setText(await fetchLog(scanId, step, controller.signal));
        setError(null);
      } catch (err) {
        if (controller.signal.aborted) return;
        setError(err instanceof Error ? err.message : 'ログを取得できませんでした。');
      }
      if (live) timer = window.setTimeout(() => void load(), 5000);
    };
    void load();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [open, live, scanId, step]);

  return (
    // <details> はブラウザ標準の「開閉できる領域」。JavaScript 無しで開閉でき、
    // キーボードや読み上げソフトにも最初から対応している。
    <details className="mt-2" onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary className="inline-block cursor-pointer py-1 text-xs text-mute underline underline-offset-4">
        処理ログ
      </summary>
      {error && <p className="mt-1 text-xs text-bad">{error}</p>}
      {text !== null && (
        <pre className="mt-1 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-md bg-ink p-2.5 font-mono text-[11px] leading-relaxed text-mute">
          {text.trim() === '' ? '（まだ出力がありません）' : text}
        </pre>
      )}
    </details>
  );
}
