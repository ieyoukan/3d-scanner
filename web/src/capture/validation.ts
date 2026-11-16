// 送信前の確認。「いま開始できるか」と、その理由・助言をまとめる。
//
// ここでの確認は利用者への案内のためのもの。同じ上限はサーバーでも必ず検証される
// （画面の確認は開発者ツールなどで迂回できるので、これだけに頼ってはいけない）。
import type { Limits } from '../api/types';

/** これより少ないと復元の品質が出にくい、という目安（Polycam の案内でも最低 20 枚）。 */
export const recommendedImages = 20;

export interface FrameSummary {
  id: string;
  bytes: number;
}

export interface Issue {
  /** error = このままでは開始できない / warn = 開始できるが品質に影響しうる */
  level: 'error' | 'warn';
  text: string;
}

export interface Check {
  canStart: boolean;
  totalBytes: number;
  issues: Issue[];
}

export function checkFrames(frames: readonly FrameSummary[], blurryCount: number, limits: Limits): Check {
  const issues: Issue[] = [];
  const totalBytes = frames.reduce((sum, f) => sum + f.bytes, 0);
  const count = frames.length;

  if (count < limits.minImages) {
    issues.push({
      level: 'error',
      text: `あと${limits.minImages - count}枚必要です（最低${limits.minImages}枚）。`,
    });
  }
  if (count > limits.maxImages) {
    issues.push({
      level: 'error',
      text: `写真が多すぎます。${count - limits.maxImages}枚減らしてください（最大${limits.maxImages}枚）。`,
    });
  }
  // multipart の区切りやメタデータのぶん、少し余裕を見る。
  const overhead = 64 * 1024 + count * 512;
  if (totalBytes + overhead > limits.maxUploadBytes) {
    issues.push({
      level: 'error',
      text: `合計サイズが上限（${formatBytes(limits.maxUploadBytes)}）を超えています。写真を減らしてください。`,
    });
  }

  if (count >= limits.minImages && count < recommendedImages) {
    issues.push({
      level: 'warn',
      text: `${recommendedImages}枚以上あると安定します。対象のまわりを少しずつ動いて撮り足すのがおすすめです。`,
    });
  }
  if (blurryCount > 0) {
    issues.push({
      level: 'warn',
      text: `ぼけている可能性のある写真が${blurryCount}枚あります。ぶれた写真は位置合わせに使えないので、削除をおすすめします。`,
    });
  }

  return { canStart: !issues.some((i) => i.level === 'error'), totalBytes, issues };
}

/** バイト数を「12.3 MB」のような表記にする。 */
export function formatBytes(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} B`;
}

/** 撮影中に出す、枚数に応じた一言。次に何をすればよいかを伝える。 */
export function captureHint(count: number, limits: Limits): string {
  if (count === 0) return '対象を画面の中央に入れて、1枚目を撮りましょう。';
  if (count < limits.minImages) return `少し横に動いて次の1枚。あと${limits.minImages - count}枚で開始できます。`;
  if (count < recommendedImages) return `開始できます。${recommendedImages}枚以上がおすすめです。高さを変えてもう1周。`;
  if (count >= limits.maxImages) return '上限に達しました。確認に進んでください。';
  return '十分な枚数です。細部を撮り足すか、確認に進みましょう。';
}
