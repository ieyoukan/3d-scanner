// アプリ全体の入口。3つのことを決める:
//   1. 最初にサーバーの設定を読む（上限値、認証が必要かどうか）
//   2. 認証が必要でまだログインしていなければ、トークン入力画面を出す
//   3. URL に応じて表示する画面を選ぶ（ルーティング）
//
// 画面は3つ:
//   /                 撮影・確認・送信
//   /scans/:id        処理の進行状況
//   /scans/:id/view   3Dビューア
import { lazy, Suspense, useCallback, useEffect, useState } from 'react';
import { BrowserRouter, Link, Route, Routes } from 'react-router-dom';
import { fetchConfig } from './api/client';
import type { ServerConfig } from './api/types';
import { TokenGate } from './auth/TokenGate';
import { CapturePage } from './pages/CapturePage';
import { ScanPage } from './pages/ScanPage';
import { Button, buttonClass } from './ui/Button';
import { SpinnerIcon } from './ui/icons';

// 3Dビューアは three.js と PlayCanvas を含み、数MBある。lazy にすると、
// ビューア画面を開くまでそのコードを読み込まない。撮影画面（モバイル回線で開くことが多い）が軽くなる。
const ViewPage = lazy(() => import('./pages/ViewPage').then((m) => ({ default: m.ViewPage })));

type Boot =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'ready'; config: ServerConfig };

export function App() {
  const [boot, setBoot] = useState<Boot>({ kind: 'loading' });

  const load = useCallback(() => {
    setBoot({ kind: 'loading' });
    fetchConfig()
      .then((config) => setBoot({ kind: 'ready', config }))
      .catch(() => setBoot({ kind: 'error', message: 'サーバーに接続できません。' }));
  }, []);

  // 起動時に1回だけ設定を読む。
  useEffect(load, [load]);

  if (boot.kind === 'loading') return <FullScreenMessage busy>読み込んでいます…</FullScreenMessage>;
  if (boot.kind === 'error') {
    return (
      <FullScreenMessage>
        <p>{boot.message}</p>
        <Button onClick={load}>もう一度試す</Button>
      </FullScreenMessage>
    );
  }

  const { config } = boot;
  if (config.authRequired && !config.authenticated) {
    // ログインできたら設定を読み直す（authenticated が true になって本体が表示される）。
    return <TokenGate onAuthenticated={load} />;
  }

  // どこかの通信が 401（Cookie の期限切れなど）を返したら、設定を読み直して入力画面に戻す。
  const onUnauthorized = load;

  return (
    <BrowserRouter>
      <Suspense fallback={<FullScreenMessage busy>3Dビューアを読み込んでいます…</FullScreenMessage>}>
        <Routes>
          <Route path="/" element={<CapturePage config={config} onUnauthorized={onUnauthorized} />} />
          <Route path="/scans/:id" element={<ScanPage config={config} onUnauthorized={onUnauthorized} />} />
          <Route path="/scans/:id/view" element={<ViewPage onUnauthorized={onUnauthorized} />} />
          <Route
            path="*"
            element={
              <FullScreenMessage>
                <p>ページが見つかりません。</p>
                <Link to="/" className={buttonClass('primary')}>
                  撮影画面へ
                </Link>
              </FullScreenMessage>
            }
          />
        </Routes>
      </Suspense>
    </BrowserRouter>
  );
}

function FullScreenMessage({ children, busy = false }: { children: React.ReactNode; busy?: boolean }) {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-4 px-6 text-center text-sm text-mute">
      {busy && <SpinnerIcon width={24} height={24} />}
      {children}
    </main>
  );
}
