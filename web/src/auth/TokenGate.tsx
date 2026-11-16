// アクセストークンの入力画面。
//
// サーバーが認証を要求していて、まだログインしていないときにだけ表示される。
// トークンはサーバーに送って Cookie に替えてもらうだけで、ブラウザには保存しない
// （localStorage に置くと、ページ内の JavaScript から読めてしまう）。
import { useState, type FormEvent } from 'react';
import { ApiError, login } from '../api/client';
import { Button } from '../ui/Button';
import { AlertIcon, CubeIcon } from '../ui/icons';

export function TokenGate({ onAuthenticated }: { onAuthenticated: () => void }) {
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (event: FormEvent) => {
    // フォームの既定の動作（ページを再読み込みして送信）を止め、自分で通信する。
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(token.trim());
      onAuthenticated();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '通信に失敗しました。もう一度お試しください。');
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="mx-auto flex min-h-dvh max-w-sm flex-col justify-center gap-6 px-6">
      <div className="space-y-2">
        <CubeIcon width={32} height={32} className="text-amber" />
        <h1 className="text-xl font-semibold">3D Scanner</h1>
        <p className="text-sm text-mute">
          このサーバーは利用者を限定しています。管理者から受け取ったアクセストークンを入力してください。
        </p>
      </div>
      <form onSubmit={(e) => void submit(e)} className="space-y-3">
        <label className="block space-y-1.5">
          <span className="text-sm text-mute">アクセストークン</span>
          <input
            // type="password": 入力中の文字を画面に出さない。パスワード管理ソフトにも保存できる。
            type="password"
            autoComplete="current-password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            className="min-h-11 w-full rounded-lg border border-line bg-panel px-3 font-mono text-base outline-none focus:border-amber"
          />
        </label>
        {error && (
          <p role="alert" className="flex items-start gap-2 text-sm text-bad">
            <AlertIcon className="mt-0.5 shrink-0" />
            <span>{error}</span>
          </p>
        )}
        <Button type="submit" variant="primary" className="w-full" disabled={busy || token.trim() === ''}>
          {busy ? '確認しています…' : '入る'}
        </Button>
      </form>
    </main>
  );
}
