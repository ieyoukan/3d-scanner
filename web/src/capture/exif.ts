// JPEG の EXIF から「35mm 換算の焦点距離」（FocalLengthIn35mmFilm、タグ 0xA405）だけを読む。
//
// なぜ要るのか: SfM はカメラの焦点距離も推定するが、良い初期値があると写真どうしの位置関係を
// 解きやすくなる。スマホのカメラアプリの JPEG にはこの値が入っているが、送る前に縮小し直すと
// EXIF は消えてしまう。そこで縮小の前に読んでおき、メタデータとしてサーバーに送る。
//
// EXIF の形（必要な所だけ）:
//   JPEG = FFD8, セグメント（FFxx + 長さ2バイト + 中身）の並び
//   APP1（FFE1）の中身 = "Exif\0\0" + TIFF
//   TIFF = バイト順（"II" = リトルエンディアン / "MM" = ビッグ）+ 0x002A + IFD0 の位置
//   IFD  = 項目数（2バイト）+ 項目（12バイト: タグ2・型2・個数4・値または位置4）の並び
//   IFD0 のタグ 0x8769 が Exif IFD の位置を指し、その中に 0xA405 がある（型 SHORT、値は項目の中に直接入る）。
//
// ライブラリを使わないのは、要るのがこの1項目だけで、数十行で読めるため。

/** EXIF を探す範囲。EXIF はファイルの先頭にあり、埋め込みサムネイルを含めても通常 64KB 以内。 */
export const exifSearchBytes = 256 * 1024;

const tagExifIFD = 0x8769;
const tagFocal35 = 0xa405;
const typeShort = 3;

/** JPEG の先頭部分から 35mm 換算の焦点距離（mm）を読む。無い・読めないときは null。 */
export function readFocalLength35mm(buffer: ArrayBuffer): number | null {
  const view = new DataView(buffer);
  if (view.byteLength < 4 || view.getUint16(0) !== 0xffd8) return null;
  let offset = 2;
  while (offset + 4 <= view.byteLength) {
    if (view.getUint8(offset) !== 0xff) return null;
    const marker = view.getUint8(offset + 1);
    // SOS（画素データの始まり）・EOI まで来たら、それ以降に EXIF は無い。
    if (marker === 0xda || marker === 0xd9) return null;
    const length = view.getUint16(offset + 2);
    if (length < 2) return null;
    const start = offset + 4;
    if (marker === 0xe1 && start + 6 <= view.byteLength && isExifHeader(view, start)) {
      return readTiff(view, start + 6, Math.min(view.byteLength, offset + 2 + length));
    }
    offset += 2 + length;
  }
  return null;
}

function isExifHeader(view: DataView, at: number): boolean {
  // "Exif\0\0"
  return (
    view.getUint32(at) === 0x45786966 && view.getUint16(at + 4) === 0x0000
  );
}

function readTiff(view: DataView, tiff: number, end: number): number | null {
  if (tiff + 8 > end) return null;
  const order = view.getUint16(tiff);
  const little = order === 0x4949; // "II"
  if (!little && order !== 0x4d4d) return null; // "MM"
  const u16 = (at: number) => view.getUint16(at, little);
  const u32 = (at: number) => view.getUint32(at, little);
  if (u16(tiff + 2) !== 0x002a) return null;

  // IFD の中から tag を探し、項目の先頭位置を返す。
  const findEntry = (ifd: number, tag: number): number | null => {
    if (ifd + 2 > end) return null;
    const count = u16(ifd);
    for (let i = 0; i < count; i++) {
      const entry = ifd + 2 + i * 12;
      if (entry + 12 > end) return null;
      if (u16(entry) === tag) return entry;
    }
    return null;
  };

  const ifd0 = tiff + u32(tiff + 4);
  const exifPointer = findEntry(ifd0, tagExifIFD);
  if (exifPointer === null) return null;
  const exifIFD = tiff + u32(exifPointer + 8);
  const focal = findEntry(exifIFD, tagFocal35);
  if (focal === null || u16(focal + 2) !== typeShort) return null;
  const value = u16(focal + 8);
  // 0 は「不明」の意味。現実的な範囲（超広角 8mm〜望遠 600mm）だけを使う。
  return value >= 8 && value <= 600 ? value : null;
}

/** ファイル（Blob）の先頭を読んで、35mm 換算の焦点距離を返す。JPEG 以外や読めないときは null。 */
export async function focalLength35mmOf(blob: Blob): Promise<number | null> {
  if (blob.type && blob.type !== 'image/jpeg') return null;
  try {
    return readFocalLength35mm(await blob.slice(0, exifSearchBytes).arrayBuffer());
  } catch {
    return null;
  }
}
