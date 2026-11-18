// ビューアのタブ（メッシュ / 3DGS / 点群）に、何を表示できるか・できないなら理由は何かをまとめる。
//
// 方針: 出来なかった出力のタブも消さない。押せない状態で残し、理由を表示する。
// 「タブが無い」だと、失敗したのか、そもそも作らない設定なのか、利用者には区別が付かない。
import type { Asset, AssetKind, Scan, StepName } from '../api/types';
import { splatJobText } from '../progress/stages';

export type TabKey = 'mesh' | 'splat' | 'points';

export interface ViewerTab {
  key: TabKey;
  label: string;
  /** 表示するファイルの URL。無ければ null（＝このタブは使えない）。 */
  url: string | null;
  /** 使えない理由。使えるときは空文字。 */
  reason: string;
  /** 補足（例: 圧縮版が無いので元の PLY を表示している）。 */
  note: string;
}

export interface Download {
  kind: AssetKind;
  label: string;
  fileName: string;
  sizeBytes: number;
  href: string;
}

function find(scan: Scan, kind: AssetKind): Asset | undefined {
  return scan.assets.find((a) => a.kind === kind);
}

/** あるステップが成果物を出せなかった理由を、サーバーの記録から取り出す。 */
function whyMissing(scan: Scan, steps: StepName[], fallback: string): string {
  for (const name of steps) {
    const step = scan.steps.find((s) => s.name === name);
    if (step?.status === 'failed' && step.error) return step.error;
  }
  for (const name of steps) {
    const step = scan.steps.find((s) => s.name === name);
    if (step?.status === 'skipped' && step.message) return step.message;
  }
  if (scan.status === 'queued' || scan.status === 'running') return 'まだ処理中です。';
  return fallback;
}

export function buildTabs(scan: Scan): ViewerTab[] {
  const mesh = find(scan, 'mesh_glb');
  const sog = find(scan, 'gaussian_sog');
  const ply = find(scan, 'gaussian_ply');
  const points = find(scan, 'sparse_ply');

  // 3DGS は圧縮版（SOG）を優先する。SOG への変換だけ失敗した場合は、元の PLY を表示する。
  const splat = sog ?? ply;
  let splatNote = !sog && ply ? '圧縮版（SOG）を作れなかったため、元の PLY を表示しています。読み込みに時間がかかります。' : '';
  // あとで作る 3DGS が待ち行列にある。前の 3DGS があれば（作り直し中）それを表示し、無ければ理由を出す。
  if (scan.splatJob && splat) {
    const now = scan.splatJob.status === 'running' ? '学習中' : '順番待ち';
    splatNote = `作り直しています（${now}）。出来上がるまで前の 3DGS を表示します。`;
  }
  const splatReason = scan.splatJob ? splatJobText(scan) : whyMissing(scan, ['splat', 'sfm'], '3DGS は生成されていません。');

  return [
    {
      key: 'mesh',
      label: 'メッシュ',
      url: mesh?.url ?? null,
      reason: mesh ? '' : whyMissing(scan, ['mesh', 'mvs', 'sfm'], 'メッシュは生成されていません。'),
      note: '',
    },
    {
      key: 'splat',
      label: '3DGS',
      url: splat?.url ?? null,
      reason: splat ? '' : splatReason,
      note: splatNote,
    },
    {
      key: 'points',
      label: '点群',
      url: points?.url ?? null,
      reason: points ? '' : whyMissing(scan, ['sfm'], '点群は生成されていません。'),
      note: '',
    },
  ];
}

/** 最初に開くタブ。使えるものの中で、メッシュ → 3DGS → 点群 の順に選ぶ。 */
export function defaultTab(tabs: readonly ViewerTab[]): TabKey | null {
  return tabs.find((t) => t.url !== null)?.key ?? null;
}

const downloadText: Partial<Record<AssetKind, { label: string; fileName: string }>> = {
  mesh_glb: { label: 'メッシュ（GLB）', fileName: 'mesh.glb' },
  gaussian_sog: { label: '3DGS 圧縮版（SOG）', fileName: 'scene.sog' },
  gaussian_ply: { label: '3DGS（PLY）', fileName: 'splat.ply' },
  sparse_ply: { label: 'SfM の点群（PLY）', fileName: 'sparse-points.ply' },
};

/** ダウンロードできるファイルの一覧。 */
export function buildDownloads(scan: Scan): Download[] {
  const order: AssetKind[] = ['mesh_glb', 'gaussian_sog', 'gaussian_ply', 'sparse_ply'];
  const downloads: Download[] = [];
  for (const kind of order) {
    const asset = find(scan, kind);
    const text = downloadText[kind];
    if (!asset || !text) continue;
    downloads.push({
      kind,
      label: text.label,
      fileName: text.fileName,
      sizeBytes: asset.sizeBytes,
      // ?download=1 を付けると、サーバーが「表示せず保存する」ためのヘッダを付けて返す。
      href: `${asset.url}?download=1`,
    });
  }
  return downloads;
}
