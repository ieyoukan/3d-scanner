import { describe, expect, it } from 'vitest';
import { readFocalLength35mm } from './exif';

/**
 * テスト用の最小の JPEG（SOI + APP1(EXIF) + SOS）を作る。
 * IFD0 に Exif IFD への位置、Exif IFD に FocalLengthIn35mmFilm を1つだけ持つ。
 */
function jpegWithExif(focal: number | null, little: boolean, extraBefore = 0): ArrayBuffer {
  const tiff = new ArrayBuffer(8 + 2 + 12 + 4 + 2 + 12 + 4);
  const v = new DataView(tiff);
  v.setUint16(0, little ? 0x4949 : 0x4d4d);
  v.setUint16(2, 0x002a, little);
  v.setUint32(4, 8, little); // IFD0 は TIFF の先頭から 8 バイト目
  // IFD0: 項目1つ（0x8769 → Exif IFD の位置）
  v.setUint16(8, 1, little);
  v.setUint16(10, 0x8769, little);
  v.setUint16(12, 4, little); // LONG
  v.setUint32(14, 1, little);
  const exifIFD = 8 + 2 + 12 + 4;
  v.setUint32(18, exifIFD, little);
  v.setUint32(22, 0, little); // 次の IFD は無い
  // Exif IFD: 項目1つ（0xA405、SHORT）
  v.setUint16(exifIFD, focal === null ? 0 : 1, little);
  v.setUint16(exifIFD + 2, 0xa405, little);
  v.setUint16(exifIFD + 4, 3, little);
  v.setUint32(exifIFD + 6, 1, little);
  v.setUint16(exifIFD + 10, focal ?? 0, little);

  const app1Body = new Uint8Array(6 + tiff.byteLength);
  app1Body.set([0x45, 0x78, 0x69, 0x66, 0, 0]);
  app1Body.set(new Uint8Array(tiff), 6);
  const filler = new Uint8Array(extraBefore > 0 ? 4 + extraBefore : 0);
  if (extraBefore > 0) {
    // EXIF の前に別のセグメント（APP0 = JFIF など）がある場合。
    filler.set([0xff, 0xe0, ((extraBefore + 2) >> 8) & 0xff, (extraBefore + 2) & 0xff]);
  }
  const out = new Uint8Array(2 + filler.length + 4 + app1Body.length + 2);
  out.set([0xff, 0xd8]);
  out.set(filler, 2);
  let at = 2 + filler.length;
  const len = app1Body.length + 2;
  out.set([0xff, 0xe1, (len >> 8) & 0xff, len & 0xff], at);
  out.set(app1Body, at + 4);
  at += 4 + app1Body.length;
  out.set([0xff, 0xda], at);
  return out.buffer;
}

describe('readFocalLength35mm', () => {
  it('リトルエンディアン（Android に多い）', () => {
    expect(readFocalLength35mm(jpegWithExif(26, true))).toBe(26);
  });
  it('ビッグエンディアン（iPhone など）', () => {
    expect(readFocalLength35mm(jpegWithExif(24, false))).toBe(24);
  });
  it('EXIF の前に別のセグメントがあっても読める', () => {
    expect(readFocalLength35mm(jpegWithExif(28, true, 14))).toBe(28);
  });
  it('項目が無い・0（不明）・JPEG でない・途中で切れている場合は null', () => {
    expect(readFocalLength35mm(jpegWithExif(null, true))).toBeNull();
    expect(readFocalLength35mm(jpegWithExif(0, true))).toBeNull();
    expect(readFocalLength35mm(new Uint8Array([0x89, 0x50, 0x4e, 0x47]).buffer)).toBeNull();
    expect(readFocalLength35mm(jpegWithExif(26, true).slice(0, 30))).toBeNull();
    expect(readFocalLength35mm(new ArrayBuffer(0))).toBeNull();
  });
});
