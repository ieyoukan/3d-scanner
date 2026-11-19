// Package jobs はジョブの実行管理（サービス層）を担当する。
//
// 役割:
//   - ワーカー: 待ち行列から1件ずつ取り出して復元パイプラインを走らせ、結果を DB に記録する。
//   - キャンセル: 実行中のジョブを止め、データを消す。
//   - 回復: 起動時に、前回のプロセスが途中で止めたジョブを見つけてやり直す。
//   - 掃除: 保存期限を過ぎたスキャンのファイルを消す。
//
// 同時実行は1件だけにしている。理由は3つ:
//  1. SfM も 3DGS も全コアとメモリを使う。2件同時だと両方が遅くなり、メモリ不足で落ちやすい。
//  2. 作業ディレクトリの PVC は ReadWriteOnce（1ノードからしかマウントできない）ので、
//     Pod を増やして並列にすることがそもそもできない（docs/learning/06 を参照）。
//  3. 1件ずつなら「いま何が動いているか」が常に明確で、学習・デバッグがしやすい。
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// Pipeline は復元処理の実体。本番では *reconstruction.Pipeline、テストでは偽物を渡す。
type Pipeline interface {
	// Run は位置推定からメッシュまで（あとで作る設定でなければ 3DGS も）を実行する。
	Run(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error
	// RunSplat は 3DGS（学習と圧縮）だけを実行する。あとで作る 3DGS の待ち行列から呼ぶ。
	RunSplat(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error
}

// Service はジョブの実行管理。
type Service struct {
	store store.Store
	root  *workspace.Root
	pipe  Pipeline
	cfg   config.Jobs
	log   *slog.Logger
	// now は現在時刻を返す。テストで時間を進められるよう差し替え可能にしてある。
	now func() time.Time

	// mu は running を守る。HTTP ハンドラ（キャンセル）とワーカーが別の goroutine から触るため。
	mu sync.Mutex
	// running は「実行中のスキャンID → そのジョブを止める関数」。
	running map[string]context.CancelFunc

	// wake は「新しいジョブが来た」ことをワーカーに知らせる合図。
	// 容量1のチャネルにして、合図が溜まらないようにする（何度鳴らしても「起きろ」は1回で足りる）。
	wake chan struct{}

	// splatMu は splat を守る。3DGS のワーカー、ワーカー（順番を譲らせる）、削除の3者が触る。
	splatMu sync.Mutex
	// splat は実行中の 3DGS の学習（無ければ nil）。詳しくは splat.go。
	splat *runningSplat
	// splatWake は 3DGS のワーカーを起こす合図。
	splatWake chan struct{}
}

// New は Service を作る。
func New(st store.Store, root *workspace.Root, pipe Pipeline, cfg config.Jobs, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: st, root: root, pipe: pipe, cfg: cfg, log: log,
		now:       time.Now,
		running:   map[string]context.CancelFunc{},
		wake:      make(chan struct{}, 1),
		splatWake: make(chan struct{}, 1),
	}
}

// Notify はワーカーを起こす。待ち行列に追加した直後に呼ぶ。
// 呼び出し側（HTTP ハンドラ）を待たせない: 既に合図が入っていれば何もしない。
func (s *Service) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Recover は起動時に1回呼ぶ。前回のプロセスが実行中のまま残したジョブを片付ける。
//
// なぜ必要か: Pod の再起動（ノードの再起動、デプロイ、OOM Kill）は処理の途中でも起きる。
// そのとき DB には status='running' の行が残るが、実行しているプロセスはもう居ない。
// 放置すると画面は永久に「処理中」のままになる。
//
// 副作用: やり直すジョブの途中経過ファイルを消す（写真は残す）。
func (s *Service) Recover(ctx context.Context) error {
	requeued, failed, err := s.store.RecoverInterrupted(ctx, s.cfg.MaxAttempts, s.now())
	if err != nil {
		return err
	}
	for _, id := range requeued {
		s.log.Warn("中断されていたジョブをやり直します", "scan", id)
	}
	for _, id := range failed {
		s.log.Warn("中断を繰り返したジョブを失敗にしました", "scan", id)
	}
	if len(requeued) > 0 {
		s.Notify()
	}
	s.markSavedDirs(ctx)
	return s.recoverSplats(ctx, s.log)
}

// markSavedDirs は起動時に、保存済みのスキャンのディレクトリに目印を置く
// （目印の仕組みより前に保存したスキャンの分。何度実行してもよい）。失敗してもサーバーは起動する。
func (s *Service) markSavedDirs(ctx context.Context) {
	ids, err := s.root.List()
	if err != nil {
		s.log.Warn("保存の目印を確かめられません", "error", err)
		return
	}
	marked := 0
	for _, id := range ids {
		d, err := s.store.Get(ctx, id)
		if err != nil || !d.Scan.Saved {
			continue
		}
		ws, err := s.root.Scan(id)
		if err != nil || ws.IsSaved() {
			continue
		}
		if err := ws.MarkSaved(); err != nil {
			s.log.Warn("保存の目印を置けません", "scan", id, "error", err)
			continue
		}
		marked++
	}
	if marked > 0 {
		s.log.Info("保存済みのスキャンに目印を置きました", "count", marked)
	}
}

// Run はワーカーのループ。ctx が終わるまで戻らない（goroutine として起動する）。
func (s *Service) Run(ctx context.Context) {
	for {
		worked, err := s.processNext(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// DB に繋がらないなどの一時的な障害。すぐ再試行すると障害中の DB を叩き続けるので待つ。
			s.log.Error("ジョブの取得に失敗", "error", err)
		}
		if worked {
			// 待ち行列が空になっていれば、3DGS のワーカーが学習を始められる。
			s.NotifySplat()
			continue // 待ち行列にまだあるかもしれないので、待たずに次を見る
		}
		// 合図が来るか、一定時間が経つまで待つ。定期的にも見るのは、合図を取りこぼしても
		// （例: 別の経路で DB に行が入った）ジョブが置き去りにならないようにする保険。
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

// processNext は待ち行列の先頭を1件処理する。
// worked は「処理したジョブがあった」ことを表す。
func (s *Service) processNext(ctx context.Context) (worked bool, err error) {
	sc, err := s.store.ClaimNext(ctx, s.now())
	if err != nil || sc == nil {
		return false, err
	}
	// 3DGS を学習中なら、止めて順番を譲らせる（CPU を取り合わないように）。
	s.yieldSplat()
	s.runJob(ctx, *sc)
	return true, nil
}

// runJob はジョブ1件を実行し、終わり方に応じて後始末をする。
//
// 終わり方は4通りあり、それぞれ扱いが違う:
//
//	(a) 最後まで走った         → ステップと成果物から全体の状態を決めて記録する。
//	(b) 利用者がキャンセルした → 状態はキャンセル処理が既に cancelled にしている。ファイルを消す。
//	(c) サーバーが停止する     → 何も記録しない。running のまま残し、次の起動時の Recover に任せる。
//	(d) 記録しようとしたら running でなかった（完了とキャンセルが同時）→ (b) と同じくファイルを消す。
func (s *Service) runJob(ctx context.Context, sc scan.Scan) {
	log := s.log.With("scan", sc.ID, "attempt", sc.Attempts)
	ws, err := s.root.Scan(sc.ID)
	if err != nil {
		log.Error("作業ディレクトリを決められません", "error", err)
		s.finish(ctx, sc.ID, scan.Outcome{Status: scan.StatusFailed, ErrorCode: "internal", ErrorMessage: "サーバー内部のエラーです。"})
		return
	}
	if sc.Attempts > 1 {
		// やり直しのときは、前回の途中経過を消して最初から始める。
		if err := ws.ResetDerived(); err != nil {
			log.Error("途中経過を消せません", "error", err)
		}
	}

	// このジョブ専用の ctx。キャンセル時はこれだけを取り消す（サーバー全体は止めない）。
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.running[sc.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, sc.ID)
		s.mu.Unlock()
	}()

	log.Info("ジョブを開始", "frames", sc.FrameCount)
	start := s.now()
	rep := &reporter{store: s.store, ws: ws, scanID: sc.ID, now: s.now, log: log, started: map[scan.StepName]time.Time{}}
	runErr := s.pipe.Run(jobCtx, reconstruction.Job{ID: sc.ID, WS: ws, DeferSplat: s.cfg.DeferSplat, ManualSplat: !s.cfg.SplatAuto}, rep)

	switch {
	case ctx.Err() != nil:
		// (c) サーバーの停止。DB には触らない（接続も閉じられつつある）。
		log.Warn("サーバー停止のためジョブを中断しました。次回起動時にやり直します")
		return
	case jobCtx.Err() != nil:
		// (b) 利用者のキャンセル。外部プロセスが止まった今、安全にファイルを消せる。
		log.Info("ジョブがキャンセルされました")
		s.removeFiles(ws, log)
		return
	case runErr != nil:
		// パイプラインはステップの失敗をエラーにしないので、ここに来るのは想定外の事態。
		log.Error("パイプラインが異常終了", "error", runErr)
	}

	// (a) 記録されたステップと成果物から、全体の状態を決める。
	detail, err := s.store.Get(ctx, sc.ID)
	if err != nil {
		log.Error("結果を読み出せません", "error", err)
		return
	}
	kinds := make([]scan.AssetKind, 0, len(detail.Assets))
	for _, a := range detail.Assets {
		kinds = append(kinds, a.Kind)
	}
	out := scan.Summarize(detail.Steps, kinds)
	// 見られる3Dデータが出来たものはギャラリーに保存する（保存期限で消さない）。
	out.Saved = s.cfg.KeepCompleted && out.Status.Keepable()
	if out.Saved {
		// 掃除が DB に無いディレクトリを消すとき、保存したものは残すための目印。
		// 完成を記録する前に置く（記録の直前にキャンセルされたら、ディレクトリごと消すので問題ない）。
		if err := ws.MarkSaved(); err != nil {
			log.Warn("保存の目印を置けません", "error", err)
		}
	}
	if !s.finish(ctx, sc.ID, out) {
		// (d) 記録の直前にキャンセルされていた。
		s.removeFiles(ws, log)
		return
	}
	if out.Saved {
		// 長く残すので、表示に要らない中間ファイルを消して容量を空ける。失敗しても結果には影響しない。
		// 3DGS の学習に要るもの（images/ と sparse/0/）は消さない。
		if err := ws.PruneIntermediate(); err != nil {
			log.Warn("中間ファイルを消せません", "error", err)
		}
	}
	// 3DGS をあとで作る場合（パイプラインが splat を実行せずに戻った）は、待ち行列に入れる。
	splatDeferred := out.Status.Keepable() && stepStatus(detail.Steps, scan.StepSplat) == scan.StepPending
	if splatDeferred {
		if err := s.EnqueueSplat(ctx, sc.ID); err != nil {
			log.Error("3DGS を待ち行列に入れられません", "error", err)
		}
	}
	log.Info("ジョブが終了", "status", out.Status, "saved", out.Saved, "splatDeferred", splatDeferred,
		"duration", s.now().Sub(start).Round(time.Second))
}

func stepStatus(steps []scan.Step, name scan.StepName) scan.StepStatus {
	for _, st := range steps {
		if st.Name == name {
			return st.Status
		}
	}
	return ""
}

// finish は終了状態を記録する。running でなかった場合は false。
func (s *Service) finish(ctx context.Context, id string, out scan.Outcome) bool {
	ok, err := s.store.Finish(ctx, id, out, s.now())
	if err != nil {
		s.log.Error("終了状態を記録できません", "scan", id, "error", err)
		return false
	}
	return ok
}

func (s *Service) removeFiles(ws *workspace.Scan, log *slog.Logger) {
	if err := ws.Remove(); err != nil {
		log.Error("作業ディレクトリを削除できません", "error", err)
	}
}

// cancelRunning は実行中のジョブを止める。実行中でなければ false。
func (s *Service) cancelRunning(id string) bool {
	s.mu.Lock()
	cancel, ok := s.running[id]
	s.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Removal は Remove の結果。
type Removal string

const (
	// RemovalCancelled: 処理待ち・処理中だったので中止した（行は cancelled として残る）。
	RemovalCancelled Removal = "cancelled"
	// RemovalDeleted: 既に終了していたので、データと行を削除した。
	RemovalDeleted Removal = "deleted"
)

// Remove は「このスキャンをやめる／消す」という利用者の操作を実行する（DELETE /api/scans/{id}）。
//
//   - 処理待ち・処理中 → cancelled にする。実行中なら外部プロセスを止める。画像と途中経過は消す。
//   - 終了済み         → 画像・成果物・DB の行をすべて消す。
//
// 失敗条件: スキャンが無い（store.ErrNotFound）、DB やファイルの操作に失敗。
func (s *Service) Remove(ctx context.Context, id string) (Removal, error) {
	ws, err := s.root.Scan(id)
	if err != nil {
		return "", store.ErrNotFound
	}
	// 3DGS を学習中なら先に止める（OpenSplat が書いている最中にファイルを消さない）。
	s.stopSplat(id)
	// 先に DB の状態を cancelled にする。こうしておけば、止めている最中にワーカーが完了しても
	// Finish は running の行にしか効かないので、結果が成功で上書きされることはない。
	prev, changed, err := s.store.Cancel(ctx, id, s.now())
	if err != nil {
		return "", err
	}
	if changed {
		if prev == scan.StatusRunning && s.cancelRunning(id) {
			// ファイルの削除はワーカーに任せる。外部プロセスがまだ書いている最中に消すと、
			// プロセスがファイルを作り直して残骸になることがある。
			return RemovalCancelled, nil
		}
		// queued だった（まだ誰も触っていない）ので、ここで消してよい。
		if err := ws.Remove(); err != nil {
			return "", err
		}
		return RemovalCancelled, nil
	}

	// 既に終了している。ファイルを先に消し、最後に行を消す。
	// 逆順だと、途中で失敗したとき「行は無いのにファイルだけ残る」状態になり、
	// 利用者がもう一度消そうとしても 404 になってしまう。
	if err := ws.Remove(); err != nil {
		return "", err
	}
	if err := s.store.Delete(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return RemovalDeleted, nil
}

// reporter は reconstruction.Reporter の実装。パイプラインの進行状況を DB に書く。
type reporter struct {
	store  store.Store
	ws     *workspace.Scan
	scanID string
	now    func() time.Time
	log    *slog.Logger
	// started はステップの開始時刻。終了を記録するときに一緒に書き戻す。
	started map[scan.StepName]time.Time
}

// dbContext は DB 書き込み用の ctx を作る。
//
// ジョブの ctx が取り消された後でも、「直前に終わったステップ」の記録は残したい。
// そこで取り消しは引き継がず（WithoutCancel）、代わりに短い期限を付ける。
func dbContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func (r *reporter) StepStarted(ctx context.Context, step scan.StepName) {
	dbCtx, cancel := dbContext(ctx)
	defer cancel()
	t := r.now()
	r.started[step] = t
	if err := r.store.UpdateStep(dbCtx, r.scanID, scan.Step{Name: step, Status: scan.StepRunning, StartedAt: &t}); err != nil {
		r.log.Error("ステップの開始を記録できません", "step", step, "error", err)
	}
}

func (r *reporter) StepFinished(ctx context.Context, step scan.StepName, status scan.StepStatus, message string) {
	dbCtx, cancel := dbContext(ctx)
	defer cancel()
	t := r.now()
	st := scan.Step{Name: step, Status: status, FinishedAt: &t}
	if started, ok := r.started[step]; ok {
		st.StartedAt = &started
	}
	// 失敗の理由と、成功・スキップ時の補足は別の列に入れる（画面で色や見出しを変えるため）。
	if status == scan.StepFailed {
		st.ErrorMessage = message
	} else {
		st.Message = message
	}
	if err := r.store.UpdateStep(dbCtx, r.scanID, st); err != nil {
		r.log.Error("ステップの終了を記録できません", "step", step, "error", err)
	}
}

func (r *reporter) AssetReady(ctx context.Context, kind scan.AssetKind, absPath string, sizeBytes int64) {
	dbCtx, cancel := dbContext(ctx)
	defer cancel()
	// DB には作業ディレクトリからの相対パスだけを保存する。
	rel, err := r.ws.Rel(absPath)
	if err != nil {
		r.log.Error("成果物が作業ディレクトリの外にあります", "kind", kind, "error", err)
		return
	}
	if err := r.store.PutAsset(dbCtx, r.scanID, scan.Asset{Kind: kind, RelativePath: rel, SizeBytes: sizeBytes}); err != nil {
		r.log.Error("成果物を記録できません", "kind", kind, "error", err)
	}
}
