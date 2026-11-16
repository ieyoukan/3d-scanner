import { describe, expect, it } from 'vitest';
import { checkCameraSupport, describeCameraError } from './cameraErrors';

/** ブラウザが投げる DOMException の代わり（name だけが判定に使われる）。 */
function domError(name: string): Error {
  const err = new Error(name);
  err.name = name;
  return err;
}

describe('checkCameraSupport', () => {
  it('HTTPS でないページでは、呼ぶ前に理由を説明する', () => {
    const failure = checkCameraSupport({ isSecureContext: false, hasGetUserMedia: false });
    expect(failure?.problem).toBe('insecure');
    expect(failure?.message).toContain('HTTPS');
  });

  it('対応していないブラウザ', () => {
    expect(checkCameraSupport({ isSecureContext: true, hasGetUserMedia: false })?.problem).toBe('unsupported');
  });

  it('使える環境では null', () => {
    expect(checkCameraSupport({ isSecureContext: true, hasGetUserMedia: true })).toBeNull();
  });
});

describe('describeCameraError', () => {
  it.each([
    ['NotAllowedError', 'denied'],
    ['SecurityError', 'denied'],
    ['NotFoundError', 'not-found'],
    ['OverconstrainedError', 'not-found'],
    ['NotReadableError', 'in-use'],
    ['AbortError', 'in-use'],
    ['SomethingNew', 'error'],
  ])('%s → %s', (name, problem) => {
    const failure = describeCameraError(domError(name));
    expect(failure.problem).toBe(problem);
    // どの場合も、次に取れる行動（写真を選ぶ、他のアプリを確認する）を伝える。
    expect(failure.message.length).toBeGreaterThan(10);
  });

  it('Error でない値が投げられても壊れない', () => {
    expect(describeCameraError('oops').problem).toBe('error');
  });
});
