// サーバーとの通信をまとめた薄い層。
//
// 画面のコンポーネントは fetch を直接呼ばず、ここの関数を使う。そうしておくと
//   - URL やエラーの扱いが1か所にまとまる
//   - 「どんな通信をしているか」がこのファイルを読めば分かる
//
// アプリ自体には認証が無い（前段の Cloudflare Access が行う）。Access の Cookie は
// 同じオリジンへの fetch に自動で付くので、ここで認証情報を扱うことはない。
import type {
  ApiErrorDetail,
  CreateScanResult,
  GalleryPage,
  LocalizeResult,
  Scan,
  SceneInfo,
  ServerConfig,
  StepName,
} from './types';

/** サーバーが返したエラー。code で原因を区別できる。 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/** 応答が成功でなければ ApiError を投げる。 */
export async function ensureOk(res: Response): Promise<Response> {
  if (res.ok) return res;
  throw await toApiError(res.status, await res.text());
}

/**
 * エラー応答の本文を ApiError に直す。
 * サーバーは {"error":{"code","message"}} を返すが、途中の中継（Cloudflare など）が
 * HTML のエラーページを返すこともあるので、JSON でなくても壊れないようにする。
 */
export function toApiError(status: number, body: string): ApiError {
  try {
    const parsed = JSON.parse(body) as { error?: Partial<ApiErrorDetail> };
    if (parsed.error?.message) {
      return new ApiError(status, parsed.error.code ?? 'unknown', parsed.error.message);
    }
  } catch {
    // JSON でない本文。下の既定の文言を使う。
  }
  return new ApiError(status, `http_${status}`, defaultMessage(status));
}

/** サーバーの説明が無いときに出す、ステータスコードごとの文言。 */
function defaultMessage(status: number): string {
  if (status === 401 || status === 403) return 'アクセスが許可されていません。ログインの期限が切れた可能性があります。ページを読み込み直してください。';
  if (status === 413) return 'アップロードが大きすぎます。写真の枚数を減らしてください。';
  if (status === 524 || status === 504) return 'サーバーの応答が間に合いませんでした。少し待ってからお試しください。';
  if (status >= 500) return 'サーバーでエラーが発生しました。少し待ってからお試しください。';
  return `通信に失敗しました（${status}）。`;
}

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const res = await ensureOk(await fetch(url, { signal, headers: { Accept: 'application/json' } }));
  return (await res.json()) as T;
}

/** 画面が最初に読む設定（上限値など）。 */
export function fetchConfig(signal?: AbortSignal): Promise<ServerConfig> {
  return getJSON<ServerConfig>('/api/config', signal);
}

export function fetchScan(id: string, signal?: AbortSignal): Promise<Scan> {
  return getJSON<Scan>(`/api/scans/${encodeURIComponent(id)}`, signal);
}

/** ステップの処理ログ（テキスト）を取得する。 */
export async function fetchLog(id: string, step: StepName, signal?: AbortSignal): Promise<string> {
  const res = await ensureOk(
    await fetch(`/api/scans/${encodeURIComponent(id)}/logs?step=${encodeURIComponent(step)}`, { signal }),
  );
  return res.text();
}

/** キャンセル（処理中）または削除（終了済み）。 */
export async function deleteScan(id: string): Promise<'cancelled' | 'deleted'> {
  const res = await ensureOk(await fetch(`/api/scans/${encodeURIComponent(id)}`, { method: 'DELETE' }));
  const body = (await res.json()) as { result: 'cancelled' | 'deleted' };
  return body.result;
}

/** ギャラリーの一覧を新しい順に読む。after に前回の nextCursor を渡すと続きを読む。 */
export function fetchGallery(after?: string, signal?: AbortSignal): Promise<GalleryPage> {
  const params = new URLSearchParams({ limit: '24' });
  if (after) params.set('after', after);
  return getJSON<GalleryPage>(`/api/scans?${params}`, signal);
}

/** スキャンの名前を変える。サーバーが整えた（前後の空白を除いた）名前を返す。 */
export async function renameScan(id: string, title: string): Promise<string> {
  const res = await ensureOk(
    await fetch(`/api/scans/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title }),
    }),
  );
  return ((await res.json()) as { title: string }).title;
}

/**
 * 写真1枚を、スキャンの地図に位置合わせする（VPS）。地図のどこから、どちらを向いて撮ったかが返る。
 * サーバーで COLMAP を動かすので、10〜数十秒かかる。
 */
export async function localizeFrame(scanId: string, image: Blob, signal?: AbortSignal): Promise<LocalizeResult> {
  const form = new FormData();
  form.append('image', image, image.type === 'image/png' ? 'query.png' : 'query.jpg');
  const res = await ensureOk(
    await fetch(`/api/scans/${encodeURIComponent(scanId)}/localize`, { method: 'POST', body: form, signal }),
  );
  return (await res.json()) as LocalizeResult;
}

/** 3DGS を（作り直しを含めて）あとで作る待ち行列に入れる。 */
export async function requestSplat(id: string): Promise<void> {
  await ensureOk(await fetch(`/api/scans/${encodeURIComponent(id)}/splat`, { method: 'POST' }));
}

export function fetchSceneInfo(url: string, signal?: AbortSignal): Promise<SceneInfo> {
  return getJSON<SceneInfo>(url, signal);
}

export type { CreateScanResult };
