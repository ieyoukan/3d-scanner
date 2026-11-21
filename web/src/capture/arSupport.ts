// この端末で AR（WebXR の immersive-ar）を使えるか。three.js を含む AR 本体（arSession.ts）とは分けておき、
// 撮影画面を開いただけでは AR 本体を読み込まないようにする。
export async function arSupported(): Promise<boolean> {
  try {
    return (await navigator.xr?.isSessionSupported('immersive-ar')) ?? false;
  } catch {
    return false;
  }
}
