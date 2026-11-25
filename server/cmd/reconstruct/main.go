// reconstruct は復元パイプラインだけを、DB も HTTP も無しで1回実行する学習・検証用のツール。
//
// サーバーが使うのと同じ reconstruction.Pipeline をそのまま呼ぶので、
// 「サーバーが外部CLIをどの順で、どんな引数で呼ぶか」を手元で確かめられる。
//
// 使い方（コンテナの中で）:
//
//	mkdir -p /work/images && cp 写真*.jpg /work/images/
//	reconstruct -dir /work
//
// 結果は /work/output/（sparse.ply, scene.json, mesh.glb, splat.ply, scene.sog）、
// 各CLIの出力は /work/logs/ に残る。設定はサーバーと同じ環境変数で変えられる
// （例: SPLAT_ITERATIONS=300 ENABLE_MESH=false reconstruct -dir /work）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/cmdrun"
	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

func main() {
	dir := flag.String("dir", "", "作業ディレクトリ（直下の images/ に写真を置く）")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "使い方: reconstruct -dir <作業ディレクトリ>")
		os.Exit(2)
	}
	os.Exit(run(*dir))
}

func run(dir string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.LoadPipeline(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ws, err := workspace.Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	// Ctrl+C で外部プロセスごと止める（サーバーのキャンセルと同じ経路）。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runner := cmdrun.NewExec()
	caps, err := reconstruction.DetectCapabilities(ctx, runner, cfg.ColmapBin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	caps.OpenMVS = reconstruction.DetectOpenMVS(cfg.OpenMVSDir)
	fmt.Printf("COLMAP %s（GPU対応: %v）、OpenMVS: %v\n", caps.ColmapVersion, caps.ColmapGPU, caps.OpenMVS)

	rep := &printReporter{start: time.Now(), steps: map[scan.StepName]scan.Step{}}
	pipeline := reconstruction.New(cfg, runner, caps, log)
	if err := pipeline.Run(ctx, reconstruction.Job{ID: "local", WS: ws}, rep); err != nil {
		fmt.Fprintln(os.Stderr, "中断:", err)
		return 130
	}

	// サーバーと同じ規則（scan.Summarize）で全体の状態を決めて表示する。
	var steps []scan.Step
	for _, name := range scan.StepOrder {
		steps = append(steps, rep.steps[name])
	}
	out := scan.Summarize(steps, rep.assets)
	fmt.Printf("\n全体の状態: %s", out.Status)
	if out.ErrorMessage != "" {
		fmt.Printf("（%s）", out.ErrorMessage)
	}
	fmt.Printf("\n所要時間: %s\n", time.Since(rep.start).Round(time.Second))
	if out.Status == scan.StatusFailed {
		return 1
	}
	return 0
}

// printReporter は進行状況を標準出力に表示する Reporter。
type printReporter struct {
	start   time.Time
	started time.Time
	steps   map[scan.StepName]scan.Step
	assets  []scan.AssetKind
}

func (r *printReporter) StepStarted(_ context.Context, step scan.StepName) {
	r.started = time.Now()
	fmt.Printf("\n[%s] 開始\n", step)
}

func (r *printReporter) StepFinished(_ context.Context, step scan.StepName, status scan.StepStatus, message string) {
	st := scan.Step{Name: step, Status: status}
	if status == scan.StepFailed {
		st.ErrorMessage = message
	} else {
		st.Message = message
	}
	r.steps[step] = st
	elapsed := ""
	if status != scan.StepSkipped {
		elapsed = fmt.Sprintf("（%s）", time.Since(r.started).Round(time.Second))
	}
	fmt.Printf("[%s] %s%s %s\n", step, status, elapsed, message)
}

func (r *printReporter) AssetReady(_ context.Context, kind scan.AssetKind, absPath string, sizeBytes int64) {
	r.assets = append(r.assets, kind)
	fmt.Printf("  成果物 %s: %s（%d バイト）\n", kind, absPath, sizeBytes)
}
