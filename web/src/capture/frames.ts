// 撮影・選択した写真1枚（Frame）を作る処理。ブラウザの画像機能（canvas）を使う。
//
// 1枚につき、3つのものを用意する:
//   blob      サーバーに送る本体（JPEG か PNG）
//   thumbUrl  一覧に出す小さな画像の URL
//   sharpness ぼけ判定用の数値
//
// なぜ本体をそのまま一覧に出さないのか:
//   ブラウザは <img> を表示するとき、画像を画素に展開してメモリに持つ。
//   1920×1440 の写真は展開すると約 11MB。80 枚並べるとスマホのメモリを使い切り、
//   タブが落ちることがある。一覧には 320px に縮めたサムネイルだけを使う。
import { focalLength35mmOf } from './exif';
import { laplacianVariance, toGray } from './sharpness';

export type FrameSource = 'camera' | 'file' | 'video';

export interface Frame {
  id: string;
  blob: Blob;
  thumbUrl: string;
  width: number;
  height: number;
  /** 撮影（または追加）した時刻。ISO 8601 形式。 */
  capturedAt: string;
  source: FrameSource;
  sharpness: number;
  /**
   * 写真の EXIF にあった 35mm 換算の焦点距離（mm）。無ければ null。
   * 縮小し直すと EXIF は消えるので、その前に読んでおいてメタデータで送る（SfM の初期値になる）。
   * 映像や動画から切り出したコマには EXIF が無いので、いつも null。
   */
  focalLength35mm: number | null;
  /** AR で撮ったときの、撮った瞬間のカメラの位置と向き（サーバーが実際の大きさを求めるのに使う）。 */
  arPose?: ArPose;
}

/** AR（WebXR）が測ったカメラの位置（メートル、y が上）と向き（四元数 x, y, z, w）。 */
export interface ArPose {
  /** AR を始めてから終えるまでの1回の印。AR の座標系は回ごとに違う。 */
  session: string;
  position: [number, number, number];
  orientation: [number, number, number, number];
}

/** サムネイルの長辺（画素）。 */
const thumbEdge = 320;
/** これより大きい写真は縮小して送る（長辺、画素）。 */
const maxUploadEdge = 2048;
/** これより大きいファイルは、寸法が小さくても JPEG に圧縮し直して送る。 */
const maxOriginalBytes = 3 * 1024 * 1024;

/** 映像のコマを写真にするときの大きさと画質。 */
export interface FrameEncoding {
  /** 長辺の上限（画素）。 */
  maxEdge: number;
  /** JPEG の画質（0〜1）。 */
  quality: number;
}

const defaultEncoding: FrameEncoding = { maxEdge: maxUploadEdge, quality: 0.9 };

/**
 * 風景モードの写真。枚数が多い（最大 150 枚）ので、1回の送信（Cloudflare の 100MB 以内）に収まるよう小さくする。
 * サーバーの特徴抽出は長辺 1600px に縮めてから行うので、SfM の精度は変わらない。
 */
export const sceneEncoding: FrameEncoding = { maxEdge: 1600, quality: 0.85 };

/** 利用者に説明できる読み込み失敗。 */
export class FrameError extends Error {}

let nextId = 0;
function newId(): string {
  nextId += 1;
  return `f${Date.now().toString(36)}-${nextId}`;
}

/** カメラで撮った JPEG から Frame を作る。 */
export async function frameFromCapture(blob: Blob): Promise<Frame> {
  const bitmap = await decode(blob);
  try {
    const { thumbUrl, sharpness } = await preview(bitmap);
    return {
      id: newId(),
      blob,
      thumbUrl,
      width: bitmap.width,
      height: bitmap.height,
      capturedAt: new Date().toISOString(),
      source: 'camera',
      sharpness,
      focalLength35mm: null,
    };
  } finally {
    // ImageBitmap は展開済みの画素を持つ。使い終わったらすぐ解放する。
    bitmap.close();
  }
}

/**
 * 端末から選んだ写真ファイルから Frame を作る。
 *
 * できるだけ元のファイルをそのまま送る。スマホのカメラアプリが書いた JPEG には
 * 焦点距離などの撮影情報（EXIF）が入っていて、COLMAP がカメラの初期値として使えるため。
 * ただし次の場合は canvas で JPEG に作り直す（このとき EXIF は失われる）:
 *   - JPEG/PNG 以外（HEIC など。ブラウザが表示できる形式ならここで変換できる）
 *   - 大きすぎる（寸法またはファイルサイズ）。Cloudflare 経由のアップロードは1回 100MB までなので、
 *     1枚を数百KB〜2MB 程度に抑える必要がある。
 */
export async function frameFromFile(file: File): Promise<Frame> {
  return frameFromImage(file, {
    name: `「${file.name}」`,
    source: 'file',
    // ファイルの更新時刻を撮影時刻の代わりにする（厳密ではないが、並び順の手掛かりになる）。
    capturedAt: new Date(file.lastModified || Date.now()).toISOString(),
  });
}

/**
 * カメラの静止画（ImageCapture.takePhoto の結果）から Frame を作る。
 * 映像のコマより画質が良く、端末によっては EXIF（焦点距離）も付く。大きいので写真ファイルと同じく縮小する。
 */
export async function frameFromPhoto(blob: Blob): Promise<Frame> {
  return frameFromImage(blob, { name: '撮った写真', source: 'camera', capturedAt: new Date().toISOString() });
}

/**
 * 動画の1コマ（再生中に createImageBitmap で写し取ったもの）から Frame を作る（video.ts から使う）。
 * bitmap は呼び出し側が閉じる。
 */
export async function frameFromVideoFrame(
  bitmap: ImageBitmap,
  capturedAt: string,
  encode: FrameEncoding = defaultEncoding,
): Promise<Frame> {
  const size = fit(bitmap.width, bitmap.height, encode.maxEdge);
  const blob = await toJpeg(draw(bitmap, size.width, size.height), encode.quality);
  const { thumbUrl, sharpness } = await preview(bitmap);
  return {
    id: newId(),
    blob,
    thumbUrl,
    width: size.width,
    height: size.height,
    capturedAt,
    source: 'video',
    sharpness,
    focalLength35mm: null,
  };
}

async function frameFromImage(
  file: Blob,
  meta: { name: string; source: FrameSource; capturedAt: string },
): Promise<Frame> {
  // 縮小し直すと EXIF が消えるので、先に読んでおく。
  const focalLength35mm = await focalLength35mmOf(file);
  let bitmap: ImageBitmap;
  try {
    bitmap = await decode(file);
  } catch {
    throw new FrameError(`${meta.name}を画像として読み込めませんでした。`);
  }
  try {
    const supported = file.type === 'image/jpeg' || file.type === 'image/png';
    const tooLarge = Math.max(bitmap.width, bitmap.height) > maxUploadEdge || file.size > maxOriginalBytes;
    let blob: Blob = file;
    let width = bitmap.width;
    let height = bitmap.height;
    if (!supported || tooLarge) {
      const scaled = fit(bitmap.width, bitmap.height, maxUploadEdge);
      const canvas = draw(bitmap, scaled.width, scaled.height);
      blob = await toJpeg(canvas, 0.9);
      width = scaled.width;
      height = scaled.height;
    }
    const { thumbUrl, sharpness } = await preview(bitmap);
    return {
      id: newId(),
      blob,
      thumbUrl,
      width,
      height,
      capturedAt: meta.capturedAt,
      source: meta.source,
      sharpness,
      focalLength35mm,
    };
  } finally {
    bitmap.close();
  }
}

/** Frame が持つサムネイルの URL を解放する。一覧から消すときに必ず呼ぶ。 */
export function releaseFrame(frame: Frame): void {
  // createObjectURL で作った URL は、明示的に解放するまでメモリ上の画像を掴み続ける。
  URL.revokeObjectURL(frame.thumbUrl);
}

/**
 * 画像データを画素に展開する。
 * imageOrientation: 'from-image' は、EXIF の「回転」指定を反映して展開する指定。
 * スマホを縦に構えて撮った写真は「横長の画素＋90°回転の指定」で保存されていることが多く、
 * これを付けないとサムネイルが横倒しになる。
 */
function decode(blob: Blob): Promise<ImageBitmap> {
  return createImageBitmap(blob, { imageOrientation: 'from-image' });
}

/** 長辺が maxEdge に収まるよう、縦横比を保って縮めた寸法を返す（拡大はしない）。 */
export function fit(width: number, height: number, maxEdge: number): { width: number; height: number } {
  const scale = Math.min(1, maxEdge / Math.max(width, height));
  return { width: Math.max(1, Math.round(width * scale)), height: Math.max(1, Math.round(height * scale)) };
}

function draw(bitmap: ImageBitmap, width: number, height: number): HTMLCanvasElement {
  const canvas = document.createElement('canvas');
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  if (!ctx) throw new FrameError('画像を処理できませんでした。');
  ctx.drawImage(bitmap, 0, 0, width, height);
  return canvas;
}

function toJpeg(canvas: HTMLCanvasElement, quality: number): Promise<Blob> {
  return new Promise((resolve, reject) => {
    canvas.toBlob(
      (blob) => (blob ? resolve(blob) : reject(new FrameError('画像を変換できませんでした。'))),
      'image/jpeg',
      quality,
    );
  });
}

/** サムネイルを作り、同じ縮小画像からくっきり度も計算する。 */
async function preview(bitmap: ImageBitmap): Promise<{ thumbUrl: string; sharpness: number }> {
  const size = fit(bitmap.width, bitmap.height, thumbEdge);
  const canvas = draw(bitmap, size.width, size.height);
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  if (!ctx) throw new FrameError('画像を処理できませんでした。');
  const pixels = ctx.getImageData(0, 0, size.width, size.height);
  const sharpness = laplacianVariance(toGray(pixels.data), size.width, size.height);
  const thumb = await toJpeg(canvas, 0.7);
  return { thumbUrl: URL.createObjectURL(thumb), sharpness };
}
