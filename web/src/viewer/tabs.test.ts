import { describe, expect, it } from 'vitest';
import type { Asset, AssetKind, Scan, Step, StepName, StepStatus } from '../api/types';
import { buildDownloads, buildTabs, defaultTab } from './tabs';

const asset = (kind: AssetKind, sizeBytes = 1000): Asset => ({ kind, sizeBytes, url: `/api/scans/x/assets/${kind}/file` });

function scan(assets: AssetKind[], steps: Partial<Record<StepName, Partial<Step> & { status: StepStatus }>> = {}): Scan {
  const names: StepName[] = ['sfm', 'mvs', 'mesh', 'splat', 'sog'];
  return {
    id: 'x',
    status: 'partial',
    createdAt: '',
    updatedAt: '',
    expiresAt: '',
    startedAt: null,
    finishedAt: null,
    frameCount: 10,
    saved: false,
    title: '',
    error: null,
    steps: names.map((name) => ({
      name,
      status: 'succeeded',
      startedAt: null,
      finishedAt: null,
      message: '',
      error: '',
      hasLog: false,
      ...steps[name],
    })),
    assets: assets.map((k) => asset(k)),
  };
}

const byKey = (s: Scan) => Object.fromEntries(buildTabs(s).map((t) => [t.key, t]));

describe('buildTabs', () => {
  it('全て揃っていれば3つとも使える。3DGS は圧縮版（SOG）を優先する', () => {
    const tabs = byKey(scan(['sparse_ply', 'scene_json', 'mesh_glb', 'gaussian_ply', 'gaussian_sog']));
    expect(tabs.mesh?.url).toContain('mesh_glb');
    expect(tabs.splat?.url).toContain('gaussian_sog');
    expect(tabs.points?.url).toContain('sparse_ply');
    expect(tabs.splat?.note).toBe('');
  });

  it('SOG が無ければ PLY で表示し、その旨を伝える', () => {
    const tabs = byKey(scan(['sparse_ply', 'gaussian_ply'], { sog: { status: 'failed', error: '変換に失敗' } }));
    expect(tabs.splat?.url).toContain('gaussian_ply');
    expect(tabs.splat?.note).toContain('PLY');
  });

  it('出来なかった出力のタブも残し、サーバーが記録した失敗理由を付ける', () => {
    const tabs = byKey(
      scan(['sparse_ply', 'mesh_glb'], {
        splat: { status: 'failed', error: 'メモリ不足の可能性があります。' },
        sog: { status: 'skipped', message: '3DGS の学習が失敗したため実行していません。' },
      }),
    );
    expect(tabs.splat?.url).toBeNull();
    expect(tabs.splat?.reason).toBe('メモリ不足の可能性があります。');
    expect(tabs.mesh?.url).not.toBeNull();
  });

  it('設定で無効だった場合は、スキップの理由を出す', () => {
    const tabs = byKey(
      scan(['sparse_ply', 'gaussian_ply', 'gaussian_sog'], {
        mvs: { status: 'skipped', message: '設定でメッシュ生成が無効になっています。' },
        mesh: { status: 'skipped', message: '設定でメッシュ生成が無効になっています。' },
      }),
    );
    expect(tabs.mesh?.reason).toContain('無効');
  });
});

describe('defaultTab', () => {
  it('使えるものの中で、メッシュ → 3DGS → 点群 の順に選ぶ', () => {
    expect(defaultTab(buildTabs(scan(['sparse_ply', 'mesh_glb', 'gaussian_sog'])))).toBe('mesh');
    expect(defaultTab(buildTabs(scan(['sparse_ply', 'gaussian_sog'])))).toBe('splat');
    expect(defaultTab(buildTabs(scan(['sparse_ply'])))).toBe('points');
    expect(defaultTab(buildTabs(scan([])))).toBeNull();
  });
});

describe('buildDownloads', () => {
  it('存在するファイルだけを、決まった順番で並べる（scene.json は出さない）', () => {
    const downloads = buildDownloads(scan(['scene_json', 'sparse_ply', 'gaussian_ply', 'mesh_glb']));
    expect(downloads.map((d) => d.kind)).toEqual(['mesh_glb', 'gaussian_ply', 'sparse_ply']);
    expect(downloads[0]).toMatchObject({ fileName: 'mesh.glb', sizeBytes: 1000 });
    expect(downloads[0]?.href.endsWith('?download=1')).toBe(true);
  });
});
