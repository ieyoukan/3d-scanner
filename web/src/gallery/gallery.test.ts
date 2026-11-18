import { describe, expect, it } from 'vitest';
import { dayLabel, destination, displayTitle, formatLabels, groupByDay, statusBadge } from './gallery';

describe('displayTitle', () => {
  it('名前があればそれを使う', () => {
    expect(displayTitle({ title: 'マグカップ', createdAt: '2026-11-18T00:05:00Z' })).toBe('マグカップ');
  });
  it('名前が無ければ撮影日時（端末の時刻）', () => {
    const iso = new Date(2026, 10, 18, 9, 5).toISOString();
    expect(displayTitle({ title: '  ', createdAt: iso })).toBe('11月18日 9:05');
  });
});

describe('dayLabel / groupByDay', () => {
  const now = new Date(2026, 10, 18, 12, 0);
  const at = (d: number, h = 10) => new Date(2026, 10, d, h).toISOString();
  it('今日・昨日・それ以前・去年', () => {
    expect(dayLabel(at(18), now)).toBe('今日');
    expect(dayLabel(at(17), now)).toBe('昨日');
    expect(dayLabel(at(3), now)).toBe('11月3日');
    expect(dayLabel(new Date(2025, 0, 2).toISOString(), now)).toBe('2025年1月2日');
  });
  it('並び順を保ったまま日付でまとめる', () => {
    const groups = groupByDay([{ createdAt: at(18, 11) }, { createdAt: at(18, 9) }, { createdAt: at(17) }], now);
    expect(groups.map((g) => [g.label, g.items.length])).toEqual([
      ['今日', 2],
      ['昨日', 1],
    ]);
  });
});

describe('statusBadge / formatLabels / destination', () => {
  it('完成したものには状態を出さない', () => {
    expect(statusBadge('succeeded')).toBeNull();
    expect(statusBadge('running')?.label).toBe('処理中');
    expect(statusBadge('failed')?.tone).toBe('bad');
  });
  it('形式の名前', () => {
    expect(formatLabels(['mesh_glb', 'gaussian_sog', 'scene_json'])).toEqual(['メッシュ', '3DGS']);
    expect(formatLabels(['sparse_ply', 'scene_json'])).toEqual(['点群のみ']);
    expect(formatLabels([])).toEqual([]);
  });
  it('見られる完成品はビューアへ、それ以外は処理状況へ', () => {
    expect(destination({ id: 'a', status: 'succeeded', assetKinds: ['mesh_glb'] })).toBe('/scans/a/view');
    expect(destination({ id: 'a', status: 'partial', assetKinds: ['gaussian_sog'] })).toBe('/scans/a/view');
    expect(destination({ id: 'a', status: 'running', assetKinds: ['mesh_glb'] })).toBe('/scans/a');
    expect(destination({ id: 'a', status: 'failed', assetKinds: ['sparse_ply'] })).toBe('/scans/a');
  });
});
