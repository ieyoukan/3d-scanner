// scanner-server は 3D Scanner のサーバー本体。
//
// 1つのプロセスの中で3つの役割が動く:
//
//	HTTP API   写真の受付、状態の問い合わせ、成果物の配信、Web（SPA）の配信
//	ワーカー   待ち行列から1件ずつ取り出して COLMAP / OpenSplat 等を実行
//	掃除係     保存期限を過ぎたスキャンの削除
//
// なぜ API とワーカーを同じプロセスにするのか:
// どちらも同じ作業ディレクトリ（PVC）を読み書きする。配備先の PVC は ReadWriteOnce、
// つまり「同時に1つのノードからしかマウントできない」ので、API とワーカーを別々の Pod に
// すると同じノードに載る保証が要る。MVP では1 Pod・1プロセスにまとめて、この問題自体を避ける。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/cmdrun"
	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/httpapi"
	"github.com/ieyoukan/3d-scanner/server/internal/jobs"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/vps"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

func main() {
	// ログは JSON で標準出力へ。Kubernetes はコンテナの標準出力を集めるので、
	// ファイルに書くより扱いやすい（kubectl logs や Loki でそのまま検索できる）。
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("起動または実行に失敗しました", "error", err.Error())
		os.Exit(1)
	}
}

// run は起動から停止までの全体。エラーを返せるよう main から分けてある
// （main の中で os.Exit すると defer が実行されず、後片付けが飛ばされる）。
func run(log *slog.Logger) error {
	cfg, err := config.FromOS()
	if err != nil {
		return err
	}

	// SIGTERM（Kubernetes が Pod を止めるときの合図）か Ctrl+C で ctx が取り消される。
	// この ctx をすべての goroutine に渡しておくと、合図1つで全体が止まり始める。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 保存先（PVC）。作れない・書けないなら、最初のアップロードを待たずにここで分かる。
	root, err := workspace.New(cfg.DataDir)
	if err != nil {
		return err
	}

	pg, err := connectDatabase(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer pg.Close()
	applied, err := store.Migrate(ctx, pg.DB())
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		log.Info("マイグレーションを適用しました", "files", applied)
	}

	// 外部CLIの能力を調べる（COLMAP が GPU 対応かどうかで、メッシュの作り方が変わる）。
	runner := cmdrun.NewExec()
	caps, err := reconstruction.DetectCapabilities(ctx, runner, cfg.Pipeline.ColmapBin)
	if err != nil {
		if cfg.Pipeline.RequireTools {
			return fmt.Errorf("COLMAP を確認できません（開発用に無視するなら REQUIRE_TOOLS=false）: %w", err)
		}
		log.Warn("COLMAP を確認できません。復元ジョブは SfM で失敗します", "error", err.Error())
	}
	pipeline := reconstruction.New(cfg.Pipeline, runner, caps, log)
	info := pipelineInfo(cfg, caps, pipeline)
	log.Info("復元パイプラインの構成", "colmap", caps.ColmapVersion, "colmapGPU", caps.ColmapGPU,
		"meshMode", info.MeshMode, "splatDevice", info.SplatDevice, "splatIterations", info.SplatIterations)

	svc := jobs.New(pg, root, pipeline, cfg.Jobs, log)
	// 前回のプロセスが実行中のまま残したジョブを片付けてから、ワーカーを動かす。
	if err := svc.Recover(ctx); err != nil {
		return fmt.Errorf("中断ジョブの回復に失敗: %w", err)
	}

	// ワーカーと掃除係を goroutine として起動する。WaitGroup で「全員が終わるまで待つ」ことができる。
	var wg sync.WaitGroup
	wg.Go(func() { svc.Run(ctx) })
	wg.Go(func() { svc.RunSplats(ctx) })
	wg.Go(func() { svc.RunJanitor(ctx) })

	// 位置合わせ（VPS）。前回のプロセスが消し損ねた一時ファイルを先に片付ける。
	if err := root.CleanTemp(); err != nil {
		log.Warn("一時ファイルを片付けられません", "error", err.Error())
	}
	localizer := vps.New(cfg.Pipeline, runner, root, log)

	api := httpapi.New(cfg, pg, root, svc, localizer, info, log)
	srv := httpapi.NewHTTPServer(cfg.ListenAddr, api.Handler())
	serveErr := make(chan error, 1)
	go func() {
		log.Info("HTTP サーバーを開始", "addr", cfg.ListenAddr, "web", cfg.WebDir != "")
		// ListenAndServe は Shutdown で止めると ErrServerClosed を返す。これは正常な終了。
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	// 停止の合図か、サーバーの異常終了を待つ。
	select {
	case <-ctx.Done():
		log.Info("停止の合図を受け取りました。後片付けを始めます")
	case err := <-serveErr:
		stop()
		wg.Wait()
		return fmt.Errorf("HTTP サーバーが停止しました: %w", err)
	}

	// 後片付け（graceful shutdown）の順番:
	//  1. HTTP: 新しい接続を断り、処理中のリクエストが終わるのを待つ。
	//  2. ワーカー: ctx の取り消しで外部プロセスに SIGTERM が送られている。終わるのを待つ。
	//     実行中だったジョブは DB に running のまま残り、次の起動時にやり直される。
	//  3. DB: 最後に閉じる（defer）。ワーカーより先に閉じると、後始末の書き込みが失敗する。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP サーバーを時間内に止められませんでした", "error", err.Error())
	}
	wg.Wait()
	log.Info("停止しました")
	return nil
}

// connectDatabase は DB に接続する。起動直後は DB がまだ準備できていないことがあるので、少し粘る。
//
// Kubernetes ではアプリと PostgreSQL の Pod がほぼ同時に起動する。1回の失敗で諦めると
// コンテナが再起動を繰り返す（CrashLoopBackOff）ので、1分ほどは間隔を空けて再試行する。
func connectDatabase(ctx context.Context, url string, log *slog.Logger) (*store.Postgres, error) {
	deadline := time.Now().Add(time.Minute)
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		pg, err := store.OpenPostgres(attemptCtx, url)
		cancel()
		if err == nil {
			return pg, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, err
		}
		log.Warn("DB に接続できません。再試行します", "attempt", attempt, "error", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// pipelineInfo は画面に伝える構成情報をまとめる。
func pipelineInfo(cfg config.Config, caps reconstruction.Capabilities, p *reconstruction.Pipeline) httpapi.PipelineInfo {
	info := httpapi.PipelineInfo{
		MeshEnabled:     cfg.Pipeline.EnableMesh,
		MeshMode:        "sparse",
		SplatEnabled:    cfg.Pipeline.EnableSplat,
		SplatIterations: cfg.Pipeline.SplatIterations,
		SplatDevice:     "gpu",
		ColmapVersion:   caps.ColmapVersion,
		ColmapGPU:       caps.ColmapGPU,
		SplatDeferred:   cfg.Pipeline.EnableSplat && cfg.Jobs.DeferSplat,
	}
	if p.DenseMesh() {
		info.MeshMode = "dense"
	}
	if cfg.Pipeline.SplatForceCPU {
		info.SplatDevice = "cpu"
	}
	return info
}
