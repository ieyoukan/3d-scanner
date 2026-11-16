import { describe, expect, it } from 'vitest';
import { toApiError } from './client';
import { isTerminal } from './types';

describe('toApiError', () => {
  it('サーバーの JSON エラーから code と message を取り出す', () => {
    const err = toApiError(400, JSON.stringify({ error: { code: 'too_few_images', message: '写真が足りません。' } }));
    expect(err).toMatchObject({ status: 400, code: 'too_few_images', message: '写真が足りません。' });
  });

  it('中継サーバーが返した HTML のエラーページでも壊れない', () => {
    const err = toApiError(413, '<html><body>Request Entity Too Large</body></html>');
    expect(err.status).toBe(413);
    expect(err.code).toBe('http_413');
    expect(err.message).toContain('大きすぎ');
  });

  it('Cloudflare のタイムアウト（524）は、待ってからの再試行を促す', () => {
    expect(toApiError(524, '').message).toContain('応答が間に合いません');
  });

  it('message の無い JSON は既定の文言にする', () => {
    expect(toApiError(500, '{"error":{}}').message).toContain('サーバーでエラー');
  });
});

describe('isTerminal', () => {
  it('queued と running だけが進行中', () => {
    expect(isTerminal('queued')).toBe(false);
    expect(isTerminal('running')).toBe(false);
    for (const s of ['partial', 'succeeded', 'failed', 'cancelled', 'expired'] as const) {
      expect(isTerminal(s)).toBe(true);
    }
  });
});
