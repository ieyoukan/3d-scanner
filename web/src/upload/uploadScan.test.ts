import { describe, expect, it } from 'vitest';
import type { Frame } from '../capture/frames';
import { buildMetadata } from './uploadScan';

describe('buildMetadata', () => {
  it('写真と同じ順番で、撮影時刻と入力元を並べる', () => {
    const frame = (i: number, source: Frame['source'], focalLength35mm: number | null = null): Frame => ({
      id: `f${i}`,
      blob: new Blob(['x']),
      thumbUrl: 'blob:thumb',
      width: 1920,
      height: 1440,
      capturedAt: `2026-11-16T09:00:0${i}.000Z`,
      source,
      sharpness: 100,
      focalLength35mm,
    });
    const meta = JSON.parse(buildMetadata([frame(1, 'camera'), frame(2, 'file', 26)], { userAgent: 'test' })) as {
      client: { userAgent: string };
      frames: { capturedAt: string; source: string; width: number }[];
    };
    expect(meta.client.userAgent).toBe('test');
    expect(meta.frames).toEqual([
      { capturedAt: '2026-11-16T09:00:01.000Z', source: 'camera', width: 1920, height: 1440 },
      // 写真の EXIF にあった焦点距離は送る（無ければ項目ごと省く）。
      { capturedAt: '2026-11-16T09:00:02.000Z', source: 'file', width: 1920, height: 1440, focalLength35mm: 26 },
    ]);
    // 送信に不要な内部の値（サムネイルの URL やくっきり度）は含めない。
    expect(JSON.stringify(meta)).not.toContain('blob:thumb');
    expect(JSON.stringify(meta)).not.toContain('sharpness');
  });
});
