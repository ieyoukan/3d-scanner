// ギャラリーの表示に使う、純粋な（画面に依存しない）関数。テストしやすいよう画面から分けてある。
import type { AssetKind, GalleryItem, ScanStatus } from '../api/types';

/** 名前が無いときは撮影日時を名前の代わりにする（例: 11月18日 9:05）。2列のカードでも切れない長さにする。 */
export function displayTitle(item: { title: string; createdAt: string }): string {
  if (item.title.trim() !== '') return item.title;
  const d = new Date(item.createdAt);
  const time = `${d.getHours()}:${String(d.getMinutes()).padStart(2, '0')}`;
  return `${d.getMonth() + 1}月${d.getDate()}日 ${time}`;
}

/** 日付の見出し（同じ日のものをまとめるため）。今年なら年を省く。 */
export function dayLabel(iso: string, now = new Date()): string {
  const d = new Date(iso);
  const sameDay = (a: Date, b: Date) =>
    a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  if (sameDay(d, now)) return '今日';
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  if (sameDay(d, yesterday)) return '昨日';
  const md = `${d.getMonth() + 1}月${d.getDate()}日`;
  return d.getFullYear() === now.getFullYear() ? md : `${d.getFullYear()}年${md}`;
}

export type Tone = 'neutral' | 'active' | 'good' | 'warn' | 'bad';

/** 一覧のカードに出す状態の短い言葉。完成したもの（ふつうの状態）には何も出さない。 */
export function statusBadge(status: ScanStatus): { label: string; tone: Tone } | null {
  switch (status) {
    case 'queued':
      return { label: '順番待ち', tone: 'active' };
    case 'running':
      return { label: '処理中', tone: 'active' };
    case 'partial':
      return { label: '一部のみ', tone: 'warn' };
    case 'failed':
      return { label: '失敗', tone: 'bad' };
    case 'cancelled':
      return { label: '中止', tone: 'neutral' };
    case 'expired':
      return { label: '期限切れ', tone: 'neutral' };
    case 'succeeded':
      return null;
  }
}

/** どの形式の3Dデータがあるか（カードの下に小さく出す）。 */
export function formatLabels(kinds: AssetKind[]): string[] {
  const labels: string[] = [];
  if (kinds.includes('mesh_glb')) labels.push('メッシュ');
  if (kinds.includes('gaussian_sog') || kinds.includes('gaussian_ply')) labels.push('3DGS');
  if (labels.length === 0 && kinds.includes('sparse_ply')) labels.push('点群のみ');
  return labels;
}

/** カードを押したときの行き先。見られるものがあればビューア、無ければ処理状況の画面。 */
export function destination(item: Pick<GalleryItem, 'id' | 'assetKinds' | 'status'>): string {
  const viewable = item.assetKinds.some((k) => k === 'mesh_glb' || k === 'gaussian_sog' || k === 'gaussian_ply');
  const finished = item.status === 'succeeded' || item.status === 'partial';
  return finished && viewable ? `/scans/${item.id}/view` : `/scans/${item.id}`;
}

/** 一覧を日付ごとのまとまりに分ける（並び順は保つ）。 */
export function groupByDay<T extends { createdAt: string }>(items: T[], now = new Date()): { label: string; items: T[] }[] {
  const groups: { label: string; items: T[] }[] = [];
  for (const item of items) {
    const label = dayLabel(item.createdAt, now);
    const last = groups.at(-1);
    if (last && last.label === label) last.items.push(item);
    else groups.push({ label, items: [item] });
  }
  return groups;
}
