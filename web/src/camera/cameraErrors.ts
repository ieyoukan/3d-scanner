// カメラを使えない理由を、利用者に伝わる言葉に直す。
//
// getUserMedia の失敗は DOMException の name で種類が分かる（MDN の一覧より）。
// どの場合も「写真を選んでアップロード」に切り替えれば続けられるので、
// 画面では理由と一緒にその代替手段を必ず示す。

/** カメラが使えない理由の分類。 */
export type CameraProblem =
  | 'insecure' // HTTPS でも localhost でもない
  | 'unsupported' // ブラウザが対応していない
  | 'denied' // 利用者（または端末の設定）が拒否した
  | 'not-found' // カメラが無い
  | 'in-use' // 他のアプリが使用中など、ハードウェアにアクセスできない
  | 'error'; // その他

export interface CameraFailure {
  problem: CameraProblem;
  /** 画面に出す説明。 */
  message: string;
}

/**
 * getUserMedia を呼ぶ前に分かる問題を調べる。問題が無ければ null。
 *
 * ブラウザは、安全でないページ（http://192.168.x.x など）では navigator.mediaDevices
 * そのものを用意しない。呼んでから失敗するのではなく、先に調べて理由を説明する。
 */
export function checkCameraSupport(env: {
  isSecureContext: boolean;
  hasGetUserMedia: boolean;
}): CameraFailure | null {
  if (!env.isSecureContext) {
    return {
      problem: 'insecure',
      message:
        'このページは HTTPS ではないため、ブラウザがカメラの使用を許可しません。写真を選んでアップロードしてください。',
    };
  }
  if (!env.hasGetUserMedia) {
    return {
      problem: 'unsupported',
      message: 'このブラウザはページ内のカメラ撮影に対応していません。写真を選んでアップロードしてください。',
    };
  }
  return null;
}

/** getUserMedia が投げたエラーを分類する。 */
export function describeCameraError(err: unknown): CameraFailure {
  const name = err instanceof Error ? err.name : '';
  switch (name) {
    // 利用者が「許可しない」を選んだ、または以前に拒否した設定が残っている。
    case 'NotAllowedError':
    case 'SecurityError':
      return {
        problem: 'denied',
        message:
          'カメラの使用が許可されていません。アドレスバーの鍵アイコンから許可するか、写真を選んでアップロードしてください。',
      };
    // 条件に合うカメラが無い（PC にカメラが無い、背面カメラの指定に合わない等）。
    case 'NotFoundError':
    case 'OverconstrainedError':
      return {
        problem: 'not-found',
        message: '使えるカメラが見つかりません。写真を選んでアップロードしてください。',
      };
    // 許可はあるが、他のアプリが使用中などでハードウェアを開けない。
    case 'NotReadableError':
    case 'AbortError':
      return {
        problem: 'in-use',
        message: 'カメラを開始できません。他のアプリがカメラを使っていないか確認してください。',
      };
    default:
      return {
        problem: 'error',
        message: 'カメラを開始できませんでした。写真を選んでアップロードしてください。',
      };
  }
}
