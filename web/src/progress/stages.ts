// サーバーが返すステップ（sfm / mvs / mesh / splat / sog）を、画面に出す4つの段階にまとめる。
//
//   ① SfM          … sfm
//   ② MVS・メッシュ … mvs + mesh
//   ③ 3DGS         … splat + sog
//   ④ 完成          … スキャン全体の状態
//
// ここは「データ → 表示用のデータ」の変換だけを行う純粋な関数にしてある。
// React にも通信にも依存しないので、単体テストで全パターンを確かめられる（stages.test.ts）。
//
// 進捗率（%）は作らない。処理時間は写真の枚数や内容で大きく変わり、正しく見積もれないため。
// 代わりに「どの段階が終わり、いまどれを実行しているか」という事実だけを示す。
import type { Scan, ScanStatus, Step, StepName, StepStatus } from '../api/types';

/** 段階の表示上の状態。warning は「主な出力は出来たが、付随する処理が失敗した」。 */
export type StageStatus = StepStatus | 'warning';

export interface StepView extends Step {
  /** 日本語の名前。 */
  label: string;
  /** その処理が何をしているかの短い説明（専門用語の補足）。 */
  explanation: string;
}

export interface StageView {
  key: 'sfm' | 'mesh' | 'splat' | 'done';
  title: string;
  status: StageStatus;
  steps: StepView[];
}

const stepText: Record<StepName, { label: string; explanation: string }> = {
  sfm: {
    label: 'カメラ位置の推定（SfM）',
    explanation:
      '写真どうしで同じ点を探し、それぞれの写真を撮った位置と向き、まばらな3D点（疎な点群）を求めます。以降のすべての処理の土台です。',
  },
  mvs: {
    label: '密な復元（MVS）',
    explanation: '画素ごとに奥行きを求めて、びっしり詰まった点群を作ります。NVIDIA の GPU が必要な処理です。',
  },
  mesh: {
    label: 'メッシュ化とテクスチャ',
    explanation: '点群に面（三角形）を張り、写真から切り出した画像を貼り付けて、GLB ファイルにします。',
  },
  splat: {
    label: '3D Gaussian Splatting の学習',
    explanation:
      'シーンを大量の半透明の粒（ガウシアン）で表し、どの角度から見ても写真と同じに見えるよう、粒の位置や色を少しずつ調整します。',
  },
  sog: {
    label: '配信用に圧縮（SOG）',
    explanation: '学習結果を、ブラウザで素早く読み込める小さな形式に圧縮します。',
  },
};

function view(scan: Scan, name: StepName): StepView {
  const found = scan.steps.find((s) => s.name === name);
  const step: Step = found ?? {
    name,
    status: 'pending',
    startedAt: null,
    finishedAt: null,
    message: '',
    error: '',
    hasLog: false,
  };
  return { ...step, ...stepText[name] };
}

/**
 * 2つのステップ（主・従）から段階の状態を決める。
 * 主 = その段階の成果物を作るステップ、従 = 前処理または後処理。
 */
function combine(primary: Step, secondary: Step): StageStatus {
  if (primary.status === 'running' || secondary.status === 'running') return 'running';
  if (primary.status === 'failed') return 'failed';
  if (primary.status === 'succeeded') return secondary.status === 'failed' ? 'warning' : 'succeeded';
  // 主がまだ、または実行されなかった場合。従が失敗していたら、それが原因。
  if (secondary.status === 'failed') return 'failed';
  if (primary.status === 'skipped') return 'skipped';
  return 'pending';
}

function overall(status: ScanStatus): StageStatus {
  switch (status) {
    case 'succeeded':
      return 'succeeded';
    case 'partial':
      return 'warning';
    case 'failed':
      return 'failed';
    case 'cancelled':
    case 'expired':
      return 'skipped';
    default:
      return 'pending';
  }
}

export function buildStages(scan: Scan): StageView[] {
  const sfm = view(scan, 'sfm');
  const mvs = view(scan, 'mvs');
  const mesh = view(scan, 'mesh');
  const splat = view(scan, 'splat');
  const sog = view(scan, 'sog');
  return [
    { key: 'sfm', title: 'SfM', status: sfm.status, steps: [sfm] },
    // メッシュ段階: 成果物（GLB）を作るのは mesh。mvs はその前処理。
    { key: 'mesh', title: 'MVS・メッシュ', status: combine(mesh, mvs), steps: [mvs, mesh] },
    // 3DGS 段階: 成果物（PLY）を作るのは splat。sog はその後処理。
    { key: 'splat', title: '3DGS', status: combine(splat, sog), steps: [splat, sog] },
    { key: 'done', title: '完成', status: overall(scan.status), steps: [] },
  ];
}

export interface Headline {
  title: string;
  detail: string;
  tone: 'neutral' | 'active' | 'good' | 'warn' | 'bad';
}

/** 画面のいちばん上に出す、全体の状況の一言。 */
export function headline(scan: Scan): Headline {
  switch (scan.status) {
    case 'queued': {
      const ahead = scan.queuePosition ?? 0;
      return {
        title: '順番を待っています',
        detail: ahead > 0 ? `前に${ahead}件の処理があります。1件ずつ順番に実行します。` : 'まもなく始まります。',
        tone: 'neutral',
      };
    }
    case 'running': {
      const current = scan.steps.find((s) => s.status === 'running');
      return {
        title: '処理しています',
        detail: current ? `いま: ${stepText[current.name].label}` : '次の処理を準備しています。',
        tone: 'active',
      };
    }
    case 'succeeded':
      if (scan.splatJob) {
        return { title: '完成しました', detail: `メッシュを3Dビューアで確認できます。${splatJobText(scan)}`, tone: 'good' };
      }
      return { title: '完成しました', detail: '3Dビューアで確認できます。', tone: 'good' };
    case 'partial':
      return {
        title: '一部が完成しました',
        detail:
          (scan.error?.message ?? '失敗した処理があります。成功した出力は閲覧できます。') +
          (scan.splatJob ? splatJobText(scan) : ''),
        tone: 'warn',
      };
    case 'failed':
      return { title: '作成できませんでした', detail: scan.error?.message ?? '処理に失敗しました。', tone: 'bad' };
    case 'cancelled':
      return { title: '中止しました', detail: '写真と途中経過は削除されました。', tone: 'neutral' };
    case 'expired':
      return { title: '保存期限が過ぎました', detail: '写真と結果は削除されました。', tone: 'neutral' };
  }
}

/**
 * あとで作る 3DGS の状況を一言にする。無ければ空文字。
 * 3DGS は手が空いたときに学習し、新しいスキャンが来たらそちらを先に処理する（サーバーの設定による）。
 */
export function splatJobText(scan: Pick<Scan, 'splatJob'>): string {
  const job = scan.splatJob;
  if (!job) return '';
  if (job.status === 'running') return '3DGS を学習しています。この画面を閉じても続きます。';
  const ahead = job.queuePosition ?? 0;
  return ahead > 0
    ? `3DGS はあとで作ります（前に${ahead}件）。`
    : '3DGS はあとで作ります。新しいスキャンがあれば、そちらを先に処理します。';
}

/** ミリ秒を「3分12秒」のような表記にする。 */
export function formatDuration(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h > 0) return `${h}時間${m}分`;
  if (m > 0) return `${m}分${s}秒`;
  return `${s}秒`;
}

/** ステップの所要時間（ミリ秒）。まだ始まっていなければ null。実行中は now までの経過。 */
export function stepDuration(step: Step, now: number): number | null {
  if (!step.startedAt) return null;
  const start = Date.parse(step.startedAt);
  const end = step.finishedAt ? Date.parse(step.finishedAt) : now;
  return end - start;
}
