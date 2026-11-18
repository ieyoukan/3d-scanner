package jobs

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// splitPipeline は「メッシュまで」と「3DGS」を別々に差し替えられる偽のパイプライン。
// run は本物と同じく、job.DeferSplat なら splat / sog のステップに触らずに戻る。
type splitPipeline struct {
	t     *testing.T
	splat func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error
}

func (p splitPipeline) Run(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
	rep.StepStarted(ctx, scan.StepSfM)
	writeAsset(p.t, ctx, job, rep, scan.AssetSparsePLY, "sparse.ply")
	rep.StepFinished(ctx, scan.StepSfM, scan.StepSucceeded, "")
	rep.StepFinished(ctx, scan.StepMVS, scan.StepSkipped, "GPU なし")
	rep.StepStarted(ctx, scan.StepMesh)
	writeAsset(p.t, ctx, job, rep, scan.AssetMeshGLB, "mesh.glb")
	rep.StepFinished(ctx, scan.StepMesh, scan.StepSucceeded, "")
	if job.DeferSplat {
		return nil
	}
	return p.RunSplat(ctx, job, rep)
}

func (p splitPipeline) RunSplat(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
	return p.splat(ctx, job, rep)
}

// splatSucceeds は 3DGS の学習と圧縮が成功したことにする。
func splatSucceeds(t *testing.T) func(context.Context, reconstruction.Job, reconstruction.Reporter) error {
	return func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		rep.StepStarted(ctx, scan.StepSplat)
		writeAsset(t, ctx, job, rep, scan.AssetGaussianPLY, "splat.ply")
		rep.StepFinished(ctx, scan.StepSplat, scan.StepSucceeded, "")
		rep.StepStarted(ctx, scan.StepSOG)
		writeAsset(t, ctx, job, rep, scan.AssetGaussianSOG, "scene.sog")
		rep.StepFinished(ctx, scan.StepSOG, scan.StepSucceeded, "")
		return nil
	}
}

// newSplatEnv は「3DGS をあとで作る」設定の環境を作り、両方のワーカーを動かす。
func newSplatEnv(t *testing.T, splat func(context.Context, reconstruction.Job, reconstruction.Reporter) error) *env {
	t.Helper()
	e := newEnv(t, splitPipeline{t: t, splat: splat})
	e.svc.cfg.DeferSplat, e.svc.cfg.SplatYield, e.svc.cfg.KeepCompleted = true, true, true
	e.start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.svc.RunSplats(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return e
}

// waitFor は cond が true になるまで待つ。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s にならない", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hasAsset(d scan.Detail, kind scan.AssetKind) bool {
	for _, a := range d.Assets {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func TestDeferredSplatRunsAfterMesh(t *testing.T) {
	e := newSplatEnv(t, splatSucceeds(t))
	id, ws := e.enqueue()
	e.svc.Notify()

	// メッシュが出来た時点で完成（保存済み）になり、その後で 3DGS が作られる。
	e.waitStatus(id, scan.StatusSucceeded)
	waitFor(t, "3DGS の完成", func() bool {
		d := e.get(id)
		return d.Splat == nil && hasAsset(d, scan.AssetGaussianSOG)
	})
	d := e.get(id)
	if !d.Scan.Saved || d.Scan.Status != scan.StatusSucceeded || stepStatus(d.Steps, scan.StepSplat) != scan.StepSucceeded {
		t.Fatalf("結果 = %+v / %+v", d.Scan, d.Steps)
	}
	// 学習に使う写真と SfM の結果は残っている（保存時の掃除で消さない）。
	if !exists(filepath.Join(ws.ImagesDir(), "frame_0001.jpg")) {
		t.Fatal("写真が消えた")
	}
}

func TestDeferredSplatFailureMakesPartial(t *testing.T) {
	e := newSplatEnv(t, func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		rep.StepStarted(ctx, scan.StepSplat)
		rep.StepFinished(ctx, scan.StepSplat, scan.StepFailed, "メモリ不足の可能性があります。")
		rep.StepFinished(ctx, scan.StepSOG, scan.StepSkipped, "")
		return nil
	})
	id, _ := e.enqueue()
	e.svc.Notify()
	d := e.waitStatus(id, scan.StatusPartial)
	if d.Splat != nil || !d.Scan.Saved || !hasAsset(d, scan.AssetMeshGLB) {
		t.Fatalf("結果 = %+v（メッシュは見られるまま partial になるはず）", d)
	}
}

// TestSplatYieldsToNewScan: 学習中に新しいスキャンが来たら、学習を止めて先にそちらを処理し、学習は後でやり直す。
func TestSplatYieldsToNewScan(t *testing.T) {
	var calls atomic.Int32
	started := make(chan string, 4)
	var mu sync.Mutex
	block := true
	e := newSplatEnv(t, func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		calls.Add(1)
		rep.StepStarted(ctx, scan.StepSplat)
		started <- job.ID
		mu.Lock()
		b := block
		mu.Unlock()
		if b {
			<-ctx.Done() // 止められるまで学習し続ける
			return ctx.Err()
		}
		return splatSucceeds(t)(ctx, job, rep)
	})

	first, _ := e.enqueue()
	e.svc.Notify()
	if got := <-started; got != first {
		t.Fatalf("学習を始めたのは %s", got)
	}

	// 学習中に新しいスキャンが来る。
	mu.Lock()
	block = false
	mu.Unlock()
	second, _ := e.enqueue()
	e.svc.Notify()

	// 新しいスキャンが先に完成し、その後で両方の 3DGS が出来る。
	e.waitStatus(second, scan.StatusSucceeded)
	waitFor(t, "両方の 3DGS の完成", func() bool {
		return e.get(first).Splat == nil && e.get(second).Splat == nil &&
			hasAsset(e.get(first), scan.AssetGaussianSOG) && hasAsset(e.get(second), scan.AssetGaussianSOG)
	})
	// 最初の学習は1回譲ったので、合計3回（譲った1回＋やり直し1回＋2件目1回）呼ばれている。
	if n := calls.Load(); n != 3 {
		t.Fatalf("RunSplat の呼び出し = %d回, want 3", n)
	}
}

// TestRemoveStopsRunningSplat: 学習中のスキャンを削除すると、学習を止めてからファイルと行を消す。
func TestRemoveStopsRunningSplat(t *testing.T) {
	started := make(chan struct{}, 1)
	stopped := make(chan struct{})
	e := newSplatEnv(t, func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		rep.StepStarted(ctx, scan.StepSplat)
		started <- struct{}{}
		<-ctx.Done()
		// 止めた後もしばらくファイルを書いている外部プロセスのまね。
		time.Sleep(20 * time.Millisecond)
		os.WriteFile(filepath.Join(job.WS.OutputDir(), "splat.new.ply"), []byte("late"), 0o640)
		close(stopped)
		return ctx.Err()
	})
	id, ws := e.enqueue()
	e.svc.Notify()
	<-started

	res, err := e.svc.Remove(context.Background(), id)
	if err != nil || res != RemovalDeleted {
		t.Fatalf("Remove = %v, %v", res, err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("学習が止まる前に削除が終わった")
	}
	if exists(ws.Dir()) {
		t.Fatal("作業ディレクトリが残っている（学習が書いた残骸を含む）")
	}
	if _, err := e.store.Get(context.Background(), id); err == nil {
		t.Fatal("行が残っている")
	}
}

// TestRegenerateSplat: 完成したスキャンの 3DGS を作り直せる（GPU を使えるようになった後など）。
func TestRegenerateSplat(t *testing.T) {
	e := newSplatEnv(t, splatSucceeds(t))
	id, _ := e.enqueue()
	e.svc.Notify()
	waitFor(t, "最初の 3DGS", func() bool { return e.get(id).Splat == nil && hasAsset(e.get(id), scan.AssetGaussianSOG) })

	if err := e.svc.EnqueueSplat(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "作り直しの完了", func() bool {
		d := e.get(id)
		return d.Splat == nil && stepStatus(d.Steps, scan.StepSOG) == scan.StepSucceeded
	})
}
