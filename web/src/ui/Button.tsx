// ボタンの見た目を1か所で決める。
//
// 役割ごとに3種類だけ用意し、1つの画面で「primary は1つ」を守る。
// どれが主要な操作かが、色を見ただけで分かるようにするため。
//   primary    その画面でいちばん進めてほしい操作（琥珀色）
//   secondary  それ以外の普通の操作
//   danger     取り消せない操作（削除・中止）
//
// 高さは 44px 以上にしてある。指で確実に押せる大きさの目安。
import type { ButtonHTMLAttributes } from 'react';

type Variant = 'primary' | 'secondary' | 'danger';

const styles: Record<Variant, string> = {
  primary: 'bg-amber text-ink font-semibold hover:bg-amber-deep active:bg-amber-deep disabled:bg-raised disabled:text-mute',
  secondary: 'bg-raised text-paper border border-line hover:border-mute active:bg-line disabled:text-mute',
  danger: 'bg-transparent text-bad border border-line hover:border-bad active:bg-raised disabled:text-mute',
};

export function buttonClass(variant: Variant = 'secondary', extra = ''): string {
  return `inline-flex min-h-11 items-center justify-center gap-2 rounded-lg px-4 text-[15px] transition-colors ${styles[variant]} ${extra}`;
}

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
}

export function Button({ variant = 'secondary', className = '', type = 'button', ...rest }: Props) {
  // type="button" を既定にする。HTML の既定は submit で、フォームの中に置くと意図せず送信してしまう。
  return <button type={type} className={buttonClass(variant, className)} {...rest} />;
}
