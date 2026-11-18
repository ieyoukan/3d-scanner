package jobs

// このファイルは「あとで作る 3DGS」の待ち行列を処理するワーカーを担当する。
//
// なぜ分けるのか:
//   CPU での 3DGS の学習は数十分かかる。位置推定（SfM）とメッシュは数分で終わるので、
//   メッシュが出来た時点でスキャンを完成にして見られるようにし、3DGS は手が空いたときに作る。
//
// 新しいスキャンとの順番（SplatYield）:
//   true（CPU で学習する場合の既定）: 新しいスキャンが待っている間は学習を始めない。学習中に新しいスキャンが
//     来たら学習を止めて待ち行列に戻し、新しいスキャンの位置推定とメッシュを先に済ませる。止めた学習は
//     後で最初からやり直す（途中から再開する仕組みは OpenSplat に無い）。CPU を取り合って両方が遅くなるのを避ける。
//   false（GPU で学習する場合）: 新しいスキャンの処理と並行して学習する。
//
// 同時に学習するのは1件だけ。

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
)

// splatMaxAttempts は 3DGS の学習の最大着手回数。学習は長いので、配備のたびの再起動で中断されやすい。
// 位置推定とメッシュ（MaxAttempts）より1回多くやり直す。
const splatMaxAttempts = 3

// splatStopTimeout は、学習を止めるよう頼んでから、止まるのを待つ上限。
// 外部プロセスは SIGTERM から数秒で強制終了されるので、ふつうはすぐ止まる。
const splatStopTimeout = 30 * time.Second

// stopReason は実行中の学習を止めた理由。
type stopReason int32

const (
	notStopped stopReason = iota
	// stopYield: 新しいスキャンに順番を譲る。待ち行列に戻して後でやり直す。
	stopYield
	// stopRemove: スキャンの削除・期限切れ。やり直さない。
	stopRemove
)

// runningSplat は実行中の学習1件。
type runningSplat struct {
	id     string
	cancel context.CancelFunc
	// done は学習の後片付けまで終わったら閉じられる。
	done   chan struct{}
	reason atomic.Int32
}

func (r *runningSplat) stop(why stopReason) {
	r.reason.CompareAndSwap(int32(notStopped), int32(why))
	r.cancel()
}

func (r *runningSplat) stoppedFor() stopReason { return stopReason(r.reason.Load()) }

// NotifySplat は 3DGS のワーカーを起こす。
func (s *Service) NotifySplat() {
	select {
	case s.splatWake <- struct{}{}:
	default:
	}
}

// EnqueueSplat は、完成したスキャンの 3DGS を待ち行列に入れる（作り直しを含む）。
//
// 失敗条件: スキャンが無い（store.ErrNotFound）、完成していない・既に待ち行列にある（store.ErrConflict）。
func (s *Service) EnqueueSplat(ctx context.Context, id string) error {
	if err := s.store.EnqueueSplat(ctx, id, s.now()); err != nil {
		return err
	}
	s.NotifySplat()
	return nil
}

// RunSplats は 3DGS のワーカーのループ。ctx が終わるまで戻らない（goroutine として起動する）。
func (s *Service) RunSplats(ctx context.Context) {
	for {
		worked, err := s.processNextSplat(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Error("3DGS の待ち行列を読めません", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.splatWake:
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

// processNextSplat は待ち行列の先頭を1件学習する。worked は「学習を始めた」こと。
func (s *Service) processNextSplat(ctx context.Context) (worked bool, err error) {
	// 「新しいスキャンが待っていないか確かめる → 取り出す → 実行中として登録する」を、
	// ワーカー（yieldSplat）と同じ鍵の中で行う。こうすると、新しいスキャンが取り出された直後に
	// 学習が始まっても、ワーカーの yieldSplat が必ずそれを見つけて止められる。
	s.splatMu.Lock()
	if s.cfg.SplatYield {
		active, err := s.store.CountActive(ctx)
		if err != nil || active > 0 {
			s.splatMu.Unlock()
			return false, err
		}
	}
	sc, err := s.store.ClaimNextSplat(ctx, s.now())
	if err != nil || sc == nil {
		s.splatMu.Unlock()
		return false, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	rs := &runningSplat{id: sc.ID, cancel: cancel, done: make(chan struct{})}
	s.splat = rs
	s.splatMu.Unlock()

	defer func() {
		cancel()
		s.splatMu.Lock()
		s.splat = nil
		s.splatMu.Unlock()
		close(rs.done)
	}()
	s.runSplatJob(ctx, jobCtx, rs, *sc)
	return true, nil
}

// runSplatJob は学習1件を実行し、終わり方に応じて記録する。
//
//	(a) 最後まで走った     → ステップと成果物から全体の状態を決め直して記録する（3DGS の失敗なら partial）。
//	(b) 順番を譲った       → 待ち行列に戻す。
//	(c) 削除・期限切れ     → 何もしない（行もファイルも消される）。
//	(d) サーバーが停止する → 何もしない。次の起動時の RecoverSplats が待ち行列に戻す。
func (s *Service) runSplatJob(ctx, jobCtx context.Context, rs *runningSplat, sc scan.Scan) {
	log := s.log.With("scan", sc.ID, "splat", true)
	ws, err := s.root.Scan(sc.ID)
	if err != nil {
		log.Error("作業ディレクトリを決められません", "error", err)
		return
	}
	log.Info("3DGS の学習を開始")
	start := s.now()
	rep := &reporter{store: s.store, ws: ws, scanID: sc.ID, now: s.now, log: log, started: map[scan.StepName]time.Time{}}
	runErr := s.pipe.RunSplat(jobCtx, reconstruction.Job{ID: sc.ID, WS: ws}, rep)

	switch {
	case ctx.Err() != nil:
		log.Warn("サーバー停止のため 3DGS の学習を中断しました。次回起動時にやり直します")
		return
	case rs.stoppedFor() == stopYield:
		dbCtx, cancel := dbContext(ctx)
		defer cancel()
		if err := s.store.RequeueSplat(dbCtx, sc.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Error("3DGS を待ち行列に戻せません", "error", err)
		}
		log.Info("新しいスキャンに順番を譲りました。3DGS は後でやり直します")
		return
	case rs.stoppedFor() == stopRemove:
		log.Info("スキャンの削除のため 3DGS の学習を止めました")
		return
	case runErr != nil:
		log.Error("3DGS の学習が異常終了", "error", runErr)
	}

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
	if !out.Status.Keepable() {
		// メッシュが出来ているので通常は起きない。完成したスキャンを「失敗」に戻さない。
		out = scan.Outcome{Status: scan.StatusPartial, ErrorCode: "splat_failed", ErrorMessage: "3DGS を作れませんでした。"}
	}
	if _, err := s.store.FinishSplat(ctx, sc.ID, out, s.now()); err != nil {
		log.Error("3DGS の結果を記録できません", "error", err)
		return
	}
	log.Info("3DGS の学習が終了", "status", out.Status, "duration", s.now().Sub(start).Round(time.Second))
}

// yieldSplat は、新しいスキャンを処理する前に呼ぶ。学習中なら止めて順番を譲らせ、止まるまで待つ。
func (s *Service) yieldSplat() {
	if !s.cfg.SplatYield {
		return
	}
	s.splatMu.Lock()
	rs := s.splat
	s.splatMu.Unlock()
	if rs == nil {
		return
	}
	s.log.Info("新しいスキャンを先に処理するため、3DGS の学習を止めます", "scan", rs.id)
	rs.stop(stopYield)
	s.waitSplat(rs)
}

// stopSplat は、指定したスキャンの学習中なら止めて、止まるまで待つ（削除・期限切れの前に呼ぶ）。
// 外部プロセスがまだ書いている最中にファイルを消すと、残骸ができるため。
func (s *Service) stopSplat(id string) {
	s.splatMu.Lock()
	rs := s.splat
	s.splatMu.Unlock()
	if rs == nil || rs.id != id {
		return
	}
	rs.stop(stopRemove)
	s.waitSplat(rs)
}

func (s *Service) waitSplat(rs *runningSplat) {
	select {
	case <-rs.done:
	case <-time.After(splatStopTimeout):
		s.log.Warn("3DGS の学習が時間内に止まりませんでした", "scan", rs.id)
	}
}

// recoverSplats は起動時に、前回のプロセスが学習の途中で止まった 3DGS を待ち行列に戻す。
func (s *Service) recoverSplats(ctx context.Context, log *slog.Logger) error {
	requeued, failed, err := s.store.RecoverSplats(ctx, splatMaxAttempts, s.now())
	if err != nil {
		return err
	}
	for _, id := range requeued {
		log.Warn("中断されていた 3DGS の学習をやり直します", "scan", id)
	}
	for _, id := range failed {
		log.Warn("中断を繰り返した 3DGS の学習を失敗にしました", "scan", id)
	}
	if len(requeued) > 0 {
		s.NotifySplat()
	}
	return nil
}
