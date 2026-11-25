// AR の画面で使う、文字の代わりの絵。

/** 端末を左右にゆっくり動かす絵（AR が周りを見つけるには、端末を少し動かす必要がある）。 */
export function MovePhone() {
  return (
    <svg viewBox="0 0 64 40" width="72" height="45" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
      <g className="ar-sway">
        <rect x="24" y="6" width="16" height="28" rx="3" />
        <path d="M30 29h4" />
      </g>
      <path d="M10 20h-6m0 0l3-3m-3 3l3 3M54 20h6m0 0l-3-3m3 3l-3 3" strokeOpacity="0.7" />
    </svg>
  );
}

/** 風景モードの「横に歩く」の絵（人と左右の矢印）。 */
export function WalkSideways() {
  return (
    <svg viewBox="0 0 64 40" width="72" height="45" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <circle cx="32" cy="8" r="4" />
      <path d="M32 13v12M32 25l-5 10M32 25l5 10M24 18h16" />
      <path d="M14 22H4m0 0l4-4m-4 4l4 4M50 22h10m0 0l-4-4m4 4l-4 4" strokeOpacity="0.8" />
    </svg>
  );
}
