// アイコンは小さな SVG をそのまま書く（アイコン用のライブラリやフォントは読み込まない）。
// 線の太さと角の丸みを揃えてあるので、並べても不揃いに見えない。
// aria-hidden を付けているのは、アイコンは飾りであり、意味は隣の文字（またはボタンの aria-label）が
// 伝えるため。読み上げソフトに二重に読ませない。
import type { ReactNode, SVGProps } from 'react';

function Icon({ children, ...props }: SVGProps<SVGSVGElement> & { children: ReactNode }) {
  return (
    <svg
      viewBox="0 0 24 24"
      width="20"
      height="20"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      {...props}
    >
      {children}
    </svg>
  );
}

type Props = SVGProps<SVGSVGElement>;

export const CameraIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M4 8h3l1.5-2h7L17 8h3v11H4z" />
    <circle cx="12" cy="13" r="3.5" />
  </Icon>
);

export const ImagesIcon = (p: Props) => (
  <Icon {...p}>
    <rect x="3" y="6" width="14" height="13" rx="2" />
    <path d="M7 3h12a2 2 0 0 1 2 2v11" />
    <path d="m3 16 4-4 3 3 3-4 4 5" />
  </Icon>
);

export const TrashIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v6M14 11v6" />
  </Icon>
);

export const CloseIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M6 6l12 12M18 6 6 18" />
  </Icon>
);

export const CheckIcon = (p: Props) => (
  <Icon {...p}>
    <path d="m5 12.5 4.5 4.5L19 7.5" />
  </Icon>
);

export const DashIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M7 12h10" />
  </Icon>
);

export const BackIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M15 5 8 12l7 7" />
  </Icon>
);

export const ForwardIcon = (p: Props) => (
  <Icon {...p}>
    <path d="m9 5 7 7-7 7" />
  </Icon>
);

export const AlertIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M12 4 2.5 20h19z" />
    <path d="M12 10v4.5M12 17.2v.3" />
  </Icon>
);

export const InfoIcon = (p: Props) => (
  <Icon {...p}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 11v6M12 7.5v.3" />
  </Icon>
);

export const DownloadIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M12 4v11M7.5 10.5 12 15l4.5-4.5M5 20h14" />
  </Icon>
);

export const ResetViewIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M4 12a8 8 0 1 0 2.6-5.9" />
    <path d="M4 4v4.5h4.5" />
  </Icon>
);

export const GridIcon = (p: Props) => (
  <Icon {...p}>
    <rect x="4" y="4" width="7" height="7" rx="1.5" />
    <rect x="13" y="4" width="7" height="7" rx="1.5" />
    <rect x="4" y="13" width="7" height="7" rx="1.5" />
    <rect x="13" y="13" width="7" height="7" rx="1.5" />
  </Icon>
);

export const PencilIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M15.5 5.5l3 3L9 18l-4 1 1-4z" />
    <path d="M13.5 7.5l3 3" />
  </Icon>
);

export const CubeIcon = (p: Props) => (
  <Icon {...p}>
    <path d="m12 3 8 4.5v9L12 21l-8-4.5v-9z" />
    <path d="m4 7.5 8 4.5 8-4.5M12 12v9" />
  </Icon>
);

/** 位置合わせ（VPS）。照準の印。 */
export const LocateIcon = (p: Props) => (
  <Icon {...p}>
    <circle cx="12" cy="12" r="6" />
    <circle cx="12" cy="12" r="1.5" />
    <path d="M12 2.5V6M12 18v3.5M2.5 12H6M18 12h3.5" />
  </Icon>
);

/** 続きを撮る。カメラに「＋」。 */
export const AddPhotoIcon = (p: Props) => (
  <Icon {...p}>
    <path d="M13 6H8.5L7 8H4v11h16v-6" />
    <circle cx="11" cy="13.5" r="3" />
    <path d="M19 3v6M16 6h6" />
  </Icon>
);

/** 処理中を示す回転する弧。 */
export const SpinnerIcon = (p: Props) => (
  <Icon {...p} className={`spinner ${p.className ?? ''}`}>
    <path d="M12 3a9 9 0 1 0 9 9" />
  </Icon>
);

export const VideoIcon = (p: Props) => (
  <Icon {...p}>
    <rect x="3" y="6" width="13" height="12" rx="2" />
    <path d="M16 10.5l5-3v9l-5-3z" />
  </Icon>
);
