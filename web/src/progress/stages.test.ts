import { describe, expect, it } from 'vitest';
import type { Scan, ScanStatus, Step, StepName, StepStatus } from '../api/types';
import { buildStages, formatDuration, headline, splatJobText, stepDuration } from './stages';

function step(name: StepName, status: StepStatus, extra: Partial<Step> = {}): Step {
  return { name, status, startedAt: null, finishedAt: null, message: '', error: '', hasLog: false, ...extra };
}

function scan(status: ScanStatus, statuses: Partial<Record<StepName, StepStatus>>, extra: Partial<Scan> = {}): Scan {
  const names: StepName[] = ['sfm', 'mvs', 'mesh', 'splat', 'sog'];
  return {
    id: '00000000-0000-4000-8000-000000000000',
    status,
    createdAt: '2026-11-16T00:00:00Z',
    updatedAt: '2026-11-16T00:00:00Z',
    expiresAt: '2026-11-17T00:00:00Z',
    startedAt: null,
    finishedAt: null,
    frameCount: 24,
    saved: false,
    title: '',
    baseFrameCount: 0,
    resumable: false,
    captureMode: 'object',
    continuations: [],
    error: null,
    steps: names.map((n) => step(n, statuses[n] ?? 'pending')),
    assets: [],
    ...extra,
  };
}

const stageStatuses = (s: Scan) => buildStages(s).map((stage) => stage.status);

describe('buildStages', () => {
  it('4つの段階（SfM / MVS・メッシュ / 3DGS / 完成）にまとめる', () => {
    const stages = buildStages(scan('queued', {}));
    expect(stages.map((s) => s.title)).toEqual(['SfM', 'MVS・メッシュ', '3DGS', '完成']);
    expect(stages.map((s) => s.steps.map((st) => st.name))).toEqual([['sfm'], ['mvs', 'mesh'], ['splat', 'sog'], []]);
    // 各ステップには、専門用語を補う説明が付く。
    expect(stages[0]?.steps[0]?.explanation).toContain('位置と向き');
  });

  it('順番待ちは全て pending', () => {
    expect(stageStatuses(scan('queued', {}))).toEqual(['pending', 'pending', 'pending', 'pending']);
  });

  it('GPU が無く MVS を省いた場合: メッシュ段階はメッシュの状態に従う', () => {
    const s = scan('running', { sfm: 'succeeded', mvs: 'skipped', mesh: 'running' });
    expect(stageStatuses(s)).toEqual(['succeeded', 'running', 'pending', 'pending']);
  });

  it('全て成功（MVS は省略）', () => {
    const s = scan('succeeded', { sfm: 'succeeded', mvs: 'skipped', mesh: 'succeeded', splat: 'succeeded', sog: 'succeeded' });
    expect(stageStatuses(s)).toEqual(['succeeded', 'succeeded', 'succeeded', 'succeeded']);
  });

  it('部分成功: メッシュは成功、3DGS は失敗 → 失敗を隠さず、完成は「一部失敗」', () => {
    const s = scan('partial', { sfm: 'succeeded', mvs: 'skipped', mesh: 'succeeded', splat: 'failed', sog: 'skipped' });
    expect(stageStatuses(s)).toEqual(['succeeded', 'succeeded', 'failed', 'warning']);
  });

  it('SOG への変換だけ失敗: 3DGS 段階は「一部失敗」（PLY では見られる）', () => {
    const s = scan('partial', { sfm: 'succeeded', mvs: 'skipped', mesh: 'succeeded', splat: 'succeeded', sog: 'failed' });
    expect(stageStatuses(s)).toEqual(['succeeded', 'succeeded', 'warning', 'warning']);
  });

  it('MVS が失敗してメッシュに進めなかった場合、メッシュ段階は失敗', () => {
    const s = scan('partial', { sfm: 'succeeded', mvs: 'failed', mesh: 'skipped', splat: 'succeeded', sog: 'succeeded' });
    expect(stageStatuses(s)).toEqual(['succeeded', 'failed', 'succeeded', 'warning']);
  });

  it('SfM 失敗: 後段は実行されず、完成は失敗', () => {
    const s = scan('failed', { sfm: 'failed', mvs: 'skipped', mesh: 'skipped', splat: 'skipped', sog: 'skipped' });
    expect(stageStatuses(s)).toEqual(['failed', 'skipped', 'skipped', 'failed']);
  });

  it('サーバーがステップを返さなくても壊れない（pending として扱う）', () => {
    const s = scan('queued', {}, { steps: [] });
    expect(stageStatuses(s)).toEqual(['pending', 'pending', 'pending', 'pending']);
  });
});

describe('headline', () => {
  it('順番待ちでは、前に何件あるかを伝える', () => {
    expect(headline(scan('queued', {}, { queuePosition: 2 })).detail).toContain('2件');
    expect(headline(scan('queued', {}, { queuePosition: 0 })).detail).toContain('まもなく');
  });

  it('処理中は、いま実行しているステップ名を出す', () => {
    const h = headline(scan('running', { sfm: 'succeeded', splat: 'running' }));
    expect(h.tone).toBe('active');
    expect(h.detail).toContain('Gaussian Splatting');
  });

  it('失敗と部分成功は、サーバーが返した理由をそのまま出す', () => {
    const error = { code: 'sfm_failed', message: '写真どうしの対応が見つかりません。' };
    expect(headline(scan('failed', {}, { error }))).toMatchObject({ tone: 'bad', detail: error.message });
    expect(headline(scan('partial', {}, { error }))).toMatchObject({ tone: 'warn', detail: error.message });
  });

  it('全ての状態に見出しがある', () => {
    for (const status of ['queued', 'running', 'partial', 'succeeded', 'failed', 'cancelled', 'expired'] as const) {
      expect(headline(scan(status, {})).title.length).toBeGreaterThan(0);
    }
  });
});

describe('時間の表示', () => {
  it('formatDuration', () => {
    expect(formatDuration(0)).toBe('0秒');
    expect(formatDuration(59_400)).toBe('59秒');
    expect(formatDuration(192_000)).toBe('3分12秒');
    expect(formatDuration(3_780_000)).toBe('1時間3分');
    expect(formatDuration(-5)).toBe('0秒');
  });

  it('stepDuration: 終わったステップは実際の所要時間、実行中は現在までの経過', () => {
    const start = '2026-11-16T00:00:00Z';
    const now = Date.parse('2026-11-16T00:05:00Z');
    expect(stepDuration(step('sfm', 'succeeded', { startedAt: start, finishedAt: '2026-11-16T00:01:30Z' }), now)).toBe(90_000);
    expect(stepDuration(step('sfm', 'running', { startedAt: start }), now)).toBe(300_000);
    expect(stepDuration(step('sfm', 'pending'), now)).toBeNull();
  });
});

describe('あとで作る 3DGS', () => {
  it('完成していて 3DGS を待っている間は、見出しでそれを伝える', () => {
    const s = scan('succeeded', { sfm: 'succeeded', mvs: 'skipped', mesh: 'succeeded', splat: 'pending', sog: 'pending' }, {
      splatJob: { status: 'queued', queuePosition: 0, attempts: 0 },
    });
    const h = headline(s);
    expect(h.tone).toBe('good');
    expect(h.detail).toContain('3DGS はあとで作ります');
    expect(buildStages(s).find((st) => st.key === 'splat')?.status).toBe('pending');
  });

  it('学習中・順番待ちの言い方', () => {
    expect(splatJobText({ splatJob: { status: 'running', attempts: 1 } })).toContain('学習しています');
    expect(splatJobText({ splatJob: { status: 'queued', queuePosition: 3, attempts: 0 } })).toContain('前に3件');
    expect(splatJobText({})).toBe('');
  });
});
