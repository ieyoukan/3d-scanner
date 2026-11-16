// 3つのビューア（メッシュ・3DGS・点群）に共通の約束事。

/** ビューアが親に伝える読み込み状況。 */
export type ViewerState =
  | { kind: 'loading' }
  | { kind: 'ready'; summary: string }
  | { kind: 'error'; message: string };

/** 親（ViewPage）がビューアに頼める操作。 */
export interface ViewerHandle {
  /** 視点を最初の位置に戻す。 */
  resetView: () => void;
}

/** WebGL が使えないなど、表示そのものができないときの説明。 */
export function describeViewerError(err: unknown): string {
  const text = err instanceof Error ? err.message : String(err);
  if (/webgl|context/i.test(text)) {
    return 'このブラウザでは3D表示（WebGL）を使えません。ファイルをダウンロードして、別のソフトで開いてください。';
  }
  if (/401|unauthor/i.test(text)) return 'ログインの期限が切れました。ページを読み込み直してください。';
  if (/404|410/.test(text)) return 'ファイルが見つかりません。保存期限を過ぎて削除された可能性があります。';
  return '読み込みに失敗しました。通信状況を確認して、もう一度お試しください。';
}
