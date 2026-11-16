// カメラの起動・停止・撮影を担当する React フック。
//
// 状態遷移:
//
//   idle ──start()──▶ starting ──成功──▶ ready ──stop()──▶ idle
//                        │
//                        └──失敗──▶ unavailable（理由つき）
//
// unavailable になっても撮影画面は止まらない。呼び出し側（CaptureView）が
// 「写真を選ぶ」ボタンを前面に出して、同じ流れを続けられるようにする。
import { useCallback, useEffect, useRef, useState } from 'react';
import { checkCameraSupport, describeCameraError, type CameraFailure } from './cameraErrors';

export type CameraState =
  | { kind: 'idle' }
  | { kind: 'starting' }
  | { kind: 'ready'; width: number; height: number; label: string }
  | { kind: 'unavailable'; failure: CameraFailure };

/**
 * カメラに求める条件。
 *   facingMode ideal: 背面カメラを「できれば」使う。exact にすると、背面カメラの無い PC で
 *                     OverconstrainedError になってしまうので、希望として伝える。
 *   width/height ideal: 4:3 で長辺 1920 を希望する。SfM は特徴点が取れれば十分で、
 *                     これ以上大きくしてもアップロードと処理が重くなるだけ。
 *   audio false: 音声は使わない（マイクの許可を求めない）。
 */
const constraints: MediaStreamConstraints = {
  audio: false,
  video: {
    facingMode: { ideal: 'environment' },
    width: { ideal: 1920 },
    height: { ideal: 1440 },
  },
};

export function useCamera() {
  const [state, setState] = useState<CameraState>({ kind: 'idle' });
  const videoRef = useRef<HTMLVideoElement | null>(null);
  // MediaStream は画面の描画に関係しない値なので、state ではなく ref に持つ
  // （変えても再描画を起こさない）。
  const streamRef = useRef<MediaStream | null>(null);

  /** カメラを止めて解放する。止めないと、端末のカメラ使用中ランプが点いたままになる。 */
  const stop = useCallback(() => {
    streamRef.current?.getTracks().forEach((track) => track.stop());
    streamRef.current = null;
    if (videoRef.current) videoRef.current.srcObject = null;
    setState((prev) => (prev.kind === 'unavailable' ? prev : { kind: 'idle' }));
  }, []);

  /** カメラの使用許可を求め、映像を <video> に流す。 */
  const start = useCallback(async () => {
    const unsupported = checkCameraSupport({
      isSecureContext: window.isSecureContext,
      hasGetUserMedia: typeof navigator.mediaDevices?.getUserMedia === 'function',
    });
    if (unsupported) {
      setState({ kind: 'unavailable', failure: unsupported });
      return;
    }

    setState({ kind: 'starting' });
    try {
      // ここでブラウザが許可ダイアログを出す。利用者が答えるまで await で待つ。
      const stream = await navigator.mediaDevices.getUserMedia(constraints);
      streamRef.current = stream;

      const video = videoRef.current;
      if (!video) {
        // 許可を待っている間に画面が切り替わっていた。使わないので解放する。
        stream.getTracks().forEach((track) => track.stop());
        streamRef.current = null;
        return;
      }
      video.srcObject = stream;
      await video.play();

      const track = stream.getVideoTracks()[0];
      setState({
        kind: 'ready',
        // videoWidth/Height は実際に流れてくる映像の大きさ（希望どおりとは限らない）。
        width: video.videoWidth,
        height: video.videoHeight,
        label: track?.label ?? '',
      });
    } catch (err) {
      streamRef.current?.getTracks().forEach((track) => track.stop());
      streamRef.current = null;
      setState({ kind: 'unavailable', failure: describeCameraError(err) });
    }
  }, []);

  /**
   * いま映っている1コマを JPEG にして返す。
   *
   * <video> の現在のフレームを <canvas> に描き、それを JPEG に圧縮する。
   * 品質 0.92 は、特徴点の検出に影響する圧縮ノイズを抑えつつ、サイズを数百KBに収める値。
   */
  const capture = useCallback(async (): Promise<Blob> => {
    const video = videoRef.current;
    if (!video || video.videoWidth === 0) {
      throw new Error('カメラの映像がまだ届いていません。');
    }
    const canvas = document.createElement('canvas');
    canvas.width = video.videoWidth;
    canvas.height = video.videoHeight;
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('画像を取り出せませんでした。');
    ctx.drawImage(video, 0, 0);
    return new Promise<Blob>((resolve, reject) => {
      canvas.toBlob(
        (blob) => (blob ? resolve(blob) : reject(new Error('画像を作れませんでした。'))),
        'image/jpeg',
        0.92,
      );
    });
  }, []);

  // 画面を離れる（コンポーネントが消える）ときは必ずカメラを解放する。
  useEffect(() => stop, [stop]);

  // タブを裏に回したらカメラを止め、戻ってきたら再開する。
  // 裏で撮り続ける必要は無く、電池とプライバシーの両方のため。
  useEffect(() => {
    let resume = false;
    const onVisibility = () => {
      if (document.hidden) {
        resume = streamRef.current !== null;
        if (resume) stop();
      } else if (resume) {
        resume = false;
        void start();
      }
    };
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  }, [start, stop]);

  return { state, videoRef, start, stop, capture };
}
