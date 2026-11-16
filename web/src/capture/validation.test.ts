import { describe, expect, it } from 'vitest';
import type { Limits } from '../api/types';
import { captureHint, checkFrames, formatBytes, recommendedImages } from './validation';

const limits: Limits = {
  minImages: 8,
  maxImages: 80,
  maxImageBytes: 15 * 1024 * 1024,
  maxUploadBytes: 95 * 1024 * 1024,
  maxImagePixels: 40_000_000,
  minImageSide: 320,
};

const frames = (count: number, bytes = 500_000) => Array.from({ length: count }, (_, i) => ({ id: `f${i}`, bytes }));

describe('checkFrames', () => {
  it('最低枚数に足りないと開始できず、あと何枚かを伝える', () => {
    const check = checkFrames(frames(5), 0, limits);
    expect(check.canStart).toBe(false);
    expect(check.issues[0]).toMatchObject({ level: 'error' });
    expect(check.issues[0]?.text).toContain('あと3枚');
  });

  it('最低枚数は満たすが推奨に届かないときは、開始できるが助言を出す', () => {
    const check = checkFrames(frames(10), 0, limits);
    expect(check.canStart).toBe(true);
    expect(check.issues).toHaveLength(1);
    expect(check.issues[0]).toMatchObject({ level: 'warn' });
    expect(check.issues[0]?.text).toContain(`${recommendedImages}枚以上`);
  });

  it('推奨枚数以上で問題が無ければ、指摘は出ない', () => {
    const check = checkFrames(frames(30), 0, limits);
    expect(check).toMatchObject({ canStart: true, issues: [], totalBytes: 30 * 500_000 });
  });

  it('合計サイズが上限を超えると開始できない（Cloudflare の 100MB 制限に収めるため）', () => {
    const check = checkFrames(frames(30, 4 * 1024 * 1024), 0, limits); // 120MB
    expect(check.canStart).toBe(false);
    expect(check.issues.some((i) => i.level === 'error' && i.text.includes('合計サイズ'))).toBe(true);
  });

  it('枚数の上限を超えると開始できない', () => {
    const check = checkFrames(frames(81, 1000), 0, limits);
    expect(check.canStart).toBe(false);
    expect(check.issues[0]?.text).toContain('1枚減らして');
  });

  it('ぼけた写真があっても開始はできるが、削除を勧める', () => {
    const check = checkFrames(frames(30), 2, limits);
    expect(check.canStart).toBe(true);
    expect(check.issues[0]).toMatchObject({ level: 'warn' });
    expect(check.issues[0]?.text).toContain('2枚');
  });
});

describe('captureHint', () => {
  it('枚数に応じて、次にすることを伝える', () => {
    expect(captureHint(0, limits)).toContain('1枚目');
    expect(captureHint(3, limits)).toContain('あと5枚');
    expect(captureHint(8, limits)).toContain('開始できます');
    expect(captureHint(25, limits)).toContain('十分');
    expect(captureHint(80, limits)).toContain('上限');
  });
});

describe('formatBytes', () => {
  it('読みやすい単位にする', () => {
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(2048)).toBe('2 KB');
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB');
  });
});
