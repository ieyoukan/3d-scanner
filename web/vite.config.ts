import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';

// 開発時（npm run dev）は Vite が 5173 番で画面を配り、/api への通信だけを
// Go サーバー（8080 番）に中継する。ブラウザから見ると同じオリジンになるので、
// 本番（Go サーバーが画面も API も配る）と同じ相対パス /api/... のまま動く。
// 中継先は .env の VITE_API_TARGET で変えられる。
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, '.', 'VITE_');
  const apiTarget = env.VITE_API_TARGET ?? 'http://localhost:8080';

  return {
    plugins: [react(), tailwindcss()],
    server: {
      // host: true で LAN 内のスマホからも開ける。ただしカメラ（getUserMedia）は
      // HTTPS か localhost でしか使えないので、スマホ実機でカメラを試すには
      // HTTPS のトンネル（Cloudflare Tunnel など）を通す必要がある（README 参照）。
      host: true,
      port: 5173,
      proxy: {
        '/api': { target: apiTarget, changeOrigin: false },
      },
    },
    build: {
      target: 'es2022',
      // 3Dライブラリ（three.js / PlayCanvas）は大きいが、ビューア画面を開いたときだけ読み込む
      // （App.tsx の lazy）ので、撮影画面の表示は遅くならない。警告の閾値だけ上げておく。
      chunkSizeWarningLimit: 3000,
    },
    test: {
      // テスト対象は DOM を使わない純粋な関数だけなので、Node 環境で十分。
      environment: 'node',
      include: ['src/**/*.test.ts'],
    },
  };
});
