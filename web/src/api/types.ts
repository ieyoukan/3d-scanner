// サーバー（Go）の JSON と1対1で対応する型。
// サーバー側の定義は server/internal/httpapi/handlers.go と server/internal/scan/scan.go にある。
// 片方だけ変えると食い違うので、項目を足すときは両方を直すこと。

/** スキャン全体の状態。 */
export type ScanStatus =
  | 'queued' // 順番待ち
  | 'running' // 処理中
  | 'partial' // 一部だけ成功（例: Mesh は出来たが 3DGS は失敗）
  | 'succeeded'
  | 'failed'
  | 'cancelled'
  | 'expired'; // 保存期限が過ぎて削除済み

/** 復元パイプラインの1段階。 */
export type StepName = 'sfm' | 'mvs' | 'mesh' | 'splat' | 'sog';

export type StepStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'skipped';

/** 配信される成果物の種類。 */
export type AssetKind = 'sparse_ply' | 'scene_json' | 'mesh_glb' | 'gaussian_ply' | 'gaussian_sog';

export interface ApiErrorDetail {
  /** 画面側が分岐に使う識別子（例: too_few_images）。 */
  code: string;
  /** そのまま表示できる日本語の説明。 */
  message: string;
}

export interface Step {
  name: StepName;
  status: StepStatus;
  startedAt: string | null;
  finishedAt: string | null;
  /** 成功・スキップ時の補足。 */
  message: string;
  /** 失敗の理由。 */
  error: string;
  hasLog: boolean;
  /**
   * 実行中ステップの実測の進み具合。サーバーがツールの出力から読み取れたときだけ入る
   * （いまは 3DGS の学習だけ）。入っていないステップでは、進み具合を表示しない。
   */
  progress?: StepProgress;
}

export interface StepProgress {
  current: number;
  total: number;
  /** 数えている単位（例: ステップ）。 */
  unit: string;
}

export interface Asset {
  kind: AssetKind;
  sizeBytes: number;
  url: string;
}

export interface Scan {
  id: string;
  status: ScanStatus;
  createdAt: string;
  updatedAt: string;
  expiresAt: string;
  startedAt: string | null;
  finishedAt: string | null;
  frameCount: number;
  /** ギャラリーに保存されているか（true なら保存期限で消えない）。 */
  saved: boolean;
  /** 利用者が付けた名前。空なら撮影日時を名前の代わりに出す。 */
  title: string;
  /** ギャラリー用の縮小画像。無ければ省略。 */
  thumbnailUrl?: string;
  /** 元の版（「続きを撮る」で作った場合）。新規、または元の版が削除済みなら省略。 */
  parentId?: string;
  /** 元の版から引き継いだ写真の枚数（frameCount に含まれる）。 */
  baseFrameCount: number;
  /** 「続きを撮る」と位置合わせ（VPS）に使えるか。 */
  resumable: boolean;
  /** 撮り方（object: 物を囲んで撮った / scene: 歩きながら空間を撮った）。 */
  captureMode: CaptureMode;
  /** この版を元にした続きの版（新しい順）。 */
  continuations: Continuation[];
  /** あとで作る 3DGS が待ち行列にあればその状況。無ければ省略。 */
  splatJob?: SplatJob;
  /** 自分より前に並んでいる件数。queued のときだけ入る。 */
  queuePosition?: number;
  error: ApiErrorDetail | null;
  steps: Step[];
  assets: Asset[];
}

/** あとで作る 3DGS の状況。 */
export interface SplatJob {
  /** queued = 順番待ち（新しいスキャンの処理が先） / running = 学習中 */
  status: 'queued' | 'running';
  /** 自分より前に並んでいる 3DGS の件数（順番待ちのときだけ）。 */
  queuePosition?: number;
  attempts: number;
}

/** 続きの版1件の概要。 */
export interface Continuation {
  id: string;
  status: ScanStatus;
  createdAt: string;
  frameCount: number;
}

/** 撮り方。object は物を1つ囲んで撮る、scene は歩きながら空間（部屋・庭・街並み）を撮る。 */
export type CaptureMode = 'object' | 'scene';

export interface Limits {
  minImages: number;
  maxImages: number;
  /** 風景モード（歩きながら空間を撮る）の写真の上限。 */
  maxSceneImages: number;
  maxImageBytes: number;
  maxUploadBytes: number;
  maxImagePixels: number;
  minImageSide: number;
}

export interface PipelineInfo {
  meshEnabled: boolean;
  /** dense = 密な復元あり / sparse = 疎な点群からの簡易メッシュ */
  /** dense: COLMAP の密な復元（GPU）、openmvs: OpenMVS の密な復元（CPU）、sparse: 疎な点群からの簡易メッシュ。 */
  meshMode: 'dense' | 'openmvs' | 'sparse';
  splatEnabled: boolean;
  splatIterations: number;
  splatDevice: 'cpu' | 'gpu';
  colmapVersion: string;
  colmapGpu: boolean;
  /** true なら、3DGS はメッシュの後で（手が空いたときに）作る。 */
  splatDeferred: boolean;
  /** true なら 3DGS は自動では作らず、「3DGS を作る」を押したときに作る。 */
  splatManual?: boolean;
}

export interface ServerConfig {
  limits: Limits;
  retentionHours: number;
  /** true なら、完成したスキャンはギャラリーに保存され、保存期限で消えない。 */
  keepCompleted: boolean;
  pipeline: PipelineInfo;
}

/** ギャラリーの一覧の1件（GET /api/scans）。 */
export interface GalleryItem {
  id: string;
  status: ScanStatus;
  title: string;
  createdAt: string;
  finishedAt: string | null;
  expiresAt: string;
  saved: boolean;
  frameCount: number;
  assetKinds: AssetKind[];
  thumbnailUrl?: string;
  /** 元の版（「続きを撮る」で作った場合）。 */
  parentId?: string;
  /** 元の版から引き継いだ写真の枚数。 */
  baseFrameCount: number;
  /** 3DGS をあとで作る予定（順番待ちか学習中）か。 */
  splatPending: boolean;
}

export interface GalleryPage {
  items: GalleryItem[];
  /** 続きを読むときに渡す値。続きが無ければ省略。 */
  nextCursor?: string;
}

export interface CreateScanResult {
  id: string;
  status: ScanStatus;
  /** 処理に使う写真の枚数（続きの版では引き継いだ写真を含む）。 */
  frameCount: number;
  baseFrameCount: number;
  duplicatesSkipped: number;
  expiresAt: string;
}

/** SfM 結果の要約（output/scene.json）。ビューアが向きと初期視点を決めるのに使う。 */
export interface SceneInfo {
  totalImages: number;
  registeredImages: number;
  points: number;
  meanReprojectionError: number;
  /** 世界座標での「上」方向。 */
  up: [number, number, number];
  /** 被写体の中心の推定値。 */
  center: [number, number, number];
  /** 中心からの広がりの目安。 */
  radius: number;
  cameraPositions: [number, number, number][];
  /** 座標の 1 が何メートルか。AR で撮った写真の位置から求められたときだけある（そのとき up は重力の向き）。 */
  metersPerUnit?: number;
}

/** 位置合わせ（VPS）の結果（POST /api/scans/:id/localize）。座標は地図（scene.json と同じ）の座標系。 */
export interface LocalizeResult {
  localized: boolean;
  /** カメラの位置。位置が分からなければ省略。 */
  position?: [number, number, number];
  /** レンズの向き（単位ベクトル）。 */
  forward?: [number, number, number];
  /** 画像の下方向（単位ベクトル）。forward と合わせてカメラの向きが完全に決まる。 */
  down?: [number, number, number];
  /** その写真のカメラの画角（焦点距離と主点、画素）。 */
  camera?: { width: number; height: number; fx: number; fy: number; cx: number; cy: number };
  /** 地図の3D点と一致した特徴点の数（多いほど確か）。 */
  matches: number;
  comparedImages: number;
  mapImages: number;
  durationMs: number;
  /** そのまま表示できる説明。 */
  message: string;
}

/** 終了状態（これ以上変化しない）かどうか。 */
export function isTerminal(status: ScanStatus): boolean {
  return status !== 'queued' && status !== 'running';
}
