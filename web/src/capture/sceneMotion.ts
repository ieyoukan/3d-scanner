// 風景モード（歩きながら空間を撮る）を AR で撮るときの、動きの計算（画面に依存しない）。
//
// 写真の選び方: 物を囲むときは「1/3 秒ごと」に一番くっきりしたコマを選ぶ。風景では、立ち止まると同じ写真が
// 溜まり、速く歩くと間が空く。そこで時間の代わりに「動いた量」で区間を切る（MotionMeter）。
//   1 区間 = 前の区間の始めから 15cm 動く、または向きが 12° 変わるまで
// 15cm・12° なら、隣の写真とは 8 割ほど重なる（スマホの画角は約 65°）。立ち止まっている間は区間が進まないので、
// 同じ場所の写真は1枚（その中で一番くっきりしたもの）にしかならない。
//
// 警告（MotionWarnings）: SfM が失敗する典型の動きを見つける。
//   spin  その場で向きだけ変えている（1.5 秒で 35° 以上回ったのに、12cm も動いていない）。
//         位置が変わらないと奥行きが測れない（パノラマと同じ）
//   fast  速すぎる（0.5 秒の間に秒速 0.8m 以上、または秒速 60° 以上）。ぶれて、隣の写真との重なりも減る
// 一瞬だけ出たり消えたりしないよう、出したら 1 秒は出し続ける。

export type Vec3 = readonly [number, number, number];

const deg = (rad: number) => (rad * 180) / Math.PI;

/** 2つの視線のなす角（度）。 */
function angleBetween(a: Vec3, b: Vec3): number {
  const la = Math.hypot(...a);
  const lb = Math.hypot(...b);
  if (la < 1e-9 || lb < 1e-9) return 0;
  const cos = (a[0] * b[0] + a[1] * b[1] + a[2] * b[2]) / (la * lb);
  return deg(Math.acos(Math.min(1, Math.max(-1, cos))));
}

/** 水平の距離（メートル）。 */
const horizontal = (a: Vec3, b: Vec3) => Math.hypot(a[0] - b[0], a[2] - b[2]);

/** 動いた量を、区間を数える座標（単調に増える数）にする。 */
export class MotionMeter {
  private anchor: { pos: Vec3; fwd: Vec3 } | null = null;
  private base = 0;

  constructor(
    private readonly unitMeters = 0.15,
    private readonly unitDegrees = 12,
  ) {}

  /**
   * いまの位置と視線から座標を返す。
   * @param interval 区間の長さ（座標の単位。間引いて区間を倍にしたら、それに合わせる）
   */
  update(pos: Vec3, fwd: Vec3, interval: number): number {
    if (!this.anchor) {
      this.anchor = { pos, fwd };
      return 0;
    }
    const d = Math.hypot(pos[0] - this.anchor.pos[0], pos[1] - this.anchor.pos[1], pos[2] - this.anchor.pos[2]);
    const amount = Math.max(d / this.unitMeters, angleBetween(fwd, this.anchor.fwd) / this.unitDegrees);
    if (amount >= interval) {
      // 次の区間へ。そこからまた測る（小さな揺れが積み重なって、止まっていても進むことはない）。
      this.base += interval;
      this.anchor = { pos, fwd };
      return this.base;
    }
    return this.base + Math.min(amount, interval * 0.999);
  }
}

export type MotionWarning = 'spin' | 'fast';

interface Sample {
  t: number;
  pos: Vec3;
  fwd: Vec3;
}

/** その場で回っている・速すぎる、を見つける。t はミリ秒。 */
export class MotionWarnings {
  private samples: Sample[] = [];
  private current: MotionWarning | null = null;
  private until = 0;

  push(t: number, pos: Vec3, fwd: Vec3): void {
    this.samples.push({ t, pos, fwd });
    while (this.samples.length > 0 && t - this.samples[0]!.t > 2500) this.samples.shift();
  }

  /** いまの警告（無ければ null）。 */
  warning(now: number): MotionWarning | null {
    const latest = this.samples.at(-1);
    let found: MotionWarning | null = null;
    if (latest) {
      const spinFrom = this.oldestWithin(latest.t, 1500);
      if (
        spinFrom &&
        angleBetween(flat(spinFrom.fwd), flat(latest.fwd)) >= 35 &&
        horizontal(spinFrom.pos, latest.pos) < 0.12
      ) {
        found = 'spin';
      }
      const fastFrom = this.oldestWithin(latest.t, 500);
      if (!found && fastFrom) {
        const dt = (latest.t - fastFrom.t) / 1000;
        if (dt >= 0.3) {
          const speed = Math.hypot(...(latest.pos.map((v, i) => v - fastFrom.pos[i]!) as [number, number, number])) / dt;
          const turn = angleBetween(fastFrom.fwd, latest.fwd) / dt;
          if (speed >= 0.8 || turn >= 60) found = 'fast';
        }
      }
    }
    if (found) {
      this.current = found;
      this.until = now + 1000;
    } else if (now >= this.until) {
      this.current = null;
    }
    return this.current;
  }

  /** latest から span ミリ秒以上前のうち、一番新しい標本（span だけ前に一番近いもの）。 */
  private oldestWithin(latest: number, span: number): Sample | null {
    let best: Sample | null = null;
    for (const s of this.samples) {
      if (latest - s.t >= span) best = s;
      else break;
    }
    return best;
  }
}

/** 視線を水平にする（上下を向いたことは「回った」に数えない）。 */
function flat(v: Vec3): Vec3 {
  return [v[0], 0, v[2]];
}
