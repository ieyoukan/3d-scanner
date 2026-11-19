package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// pipelineFunc は関数を Pipeline として使うための型（テストごとに振る舞いを差し替える）。
type pipelineFunc func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error

func (f pipelineFunc) Run(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
	return f(ctx, job, rep)
}

// RunSplat は呼ばれない想定（これらのテストは 3DGS をその場で作る設定で動かす）。
func (f pipelineFunc) RunSplat(context.Context, reconstruction.Job, reconstruction.Reporter) error {
	return errors.New("pipelineFunc は RunSplat に対応していません")
}

// env はテスト1件ぶんの部品一式。
type env struct {
	t     *testing.T
	store *store.Memory
	root  *workspace.Root
	svc   *Service
	clock *clock
}

// clock は手で進められる時計。
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newEnv(t *testing.T, pipe Pipeline) *env {
	t.Helper()
	root, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	cfg := config.Jobs{
		Retention: 24 * time.Hour, ExpiredRowRetention: 7 * 24 * time.Hour,
		MaxAttempts: 2, PollInterval: 10 * time.Millisecond, JanitorInterval: time.Hour,
	}
	svc := New(st, root, pipe, cfg, nil)
	ck := &clock{now: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	svc.now = ck.Now
	return &env{t: t, store: st, root: root, svc: svc, clock: ck}
}

// enqueue は写真1枚の入ったスキャンを queued で登録する。
func (e *env) enqueue() (string, *workspace.Scan) {
	e.t.Helper()
	id, err := scan.NewID()
	if err != nil {
		e.t.Fatal(err)
	}
	ws, _ := e.root.Scan(id)
	if err := ws.Create(); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.ImagesDir(), "frame_0001.jpg"), []byte("jpeg"), 0o640); err != nil {
		e.t.Fatal(err)
	}
	now := e.clock.Now()
	// 連続して登録しても並び順が決まるよう、時計を少し進める。
	e.clock.Advance(time.Millisecond)
	if err := e.store.Create(context.Background(), scan.Scan{ID: id, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), FrameCount: 1}, nil); err != nil {
		e.t.Fatal(err)
	}
	return id, ws
}

// start はワーカーを起動する。テスト終了時に止める。
func (e *env) start() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.svc.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		<-done
	}
	e.t.Cleanup(stop)
	return stop
}

func (e *env) get(id string) scan.Detail {
	e.t.Helper()
	d, err := e.store.Get(context.Background(), id)
	if err != nil {
		e.t.Fatalf("Get(%s): %v", id, err)
	}
	return d
}

// waitStatus は状態が want になるまで待つ（一定時間で諦めて失敗にする）。
func (e *env) waitStatus(id string, want scan.Status) scan.Detail {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d := e.get(id)
		if d.Scan.Status == want {
			return d
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("状態が %q にならない（現在 %q）", want, d.Scan.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeAsset は成果物ファイルを置いて Reporter に登録する。
func writeAsset(t *testing.T, ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter, kind scan.AssetKind, name string) {
	t.Helper()
	path := filepath.Join(job.WS.OutputDir(), name)
	if err := os.WriteFile(path, []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	rep.AssetReady(ctx, kind, path, 4)
}

// succeedAll は全ステップ成功・全成果物ありのパイプライン。
func succeedAll(t *testing.T) pipelineFunc {
	return func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		rep.StepStarted(ctx, scan.StepSfM)
		writeAsset(t, ctx, job, rep, scan.AssetSparsePLY, "sparse.ply")
		rep.StepFinished(ctx, scan.StepSfM, scan.StepSucceeded, "5枚中5枚")
		rep.StepFinished(ctx, scan.StepMVS, scan.StepSkipped, "GPU なし")
		rep.StepStarted(ctx, scan.StepMesh)
		writeAsset(t, ctx, job, rep, scan.AssetMeshGLB, "mesh.glb")
		rep.StepFinished(ctx, scan.StepMesh, scan.StepSucceeded, "")
		rep.StepStarted(ctx, scan.StepSplat)
		writeAsset(t, ctx, job, rep, scan.AssetGaussianPLY, "splat.ply")
		rep.StepFinished(ctx, scan.StepSplat, scan.StepSucceeded, "")
		rep.StepStarted(ctx, scan.StepSOG)
		writeAsset(t, ctx, job, rep, scan.AssetGaussianSOG, "scene.sog")
		rep.StepFinished(ctx, scan.StepSOG, scan.StepSucceeded, "")
		return nil
	}
}

func TestWorkerRunsJobToSuccess(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	id, _ := e.enqueue()
	e.start()
	e.svc.Notify()

	d := e.waitStatus(id, scan.StatusSucceeded)
	if d.Scan.Attempts != 1 || d.Scan.StartedAt == nil || d.Scan.FinishedAt == nil || d.Scan.ErrorMessage != "" {
		t.Errorf("スキャン = %+v", d.Scan)
	}
	if len(d.Assets) != 4 {
		t.Errorf("成果物 = %+v", d.Assets)
	}
	for _, a := range d.Assets {
		// DB には相対パスだけが入る。
		if filepath.IsAbs(a.RelativePath) || filepath.Dir(a.RelativePath) != "output" {
			t.Errorf("成果物のパス = %q（output/ 配下の相対パスのはず）", a.RelativePath)
		}
	}
	sfm := d.Steps[0]
	if sfm.Status != scan.StepSucceeded || sfm.Message != "5枚中5枚" || sfm.StartedAt == nil || sfm.FinishedAt == nil {
		t.Errorf("SfM ステップ = %+v", sfm)
	}
	if mvs := d.Steps[1]; mvs.Status != scan.StepSkipped || mvs.Message != "GPU なし" || mvs.StartedAt != nil {
		t.Errorf("MVS ステップ = %+v（開始していないので StartedAt は無いはず）", mvs)
	}
}

// TestWorkerRecordsPartialAndFailure: 部分成功と失敗を、パイプラインの報告どおりに記録する。
func TestWorkerRecordsPartialAndFailure(t *testing.T) {
	t.Run("Mesh だけ成功 → partial", func(t *testing.T) {
		e := newEnv(t, pipelineFunc(func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
			rep.StepFinished(ctx, scan.StepSfM, scan.StepSucceeded, "")
			rep.StepFinished(ctx, scan.StepMVS, scan.StepSkipped, "")
			writeAsset(t, ctx, job, rep, scan.AssetMeshGLB, "mesh.glb")
			rep.StepFinished(ctx, scan.StepMesh, scan.StepSucceeded, "")
			rep.StepStarted(ctx, scan.StepSplat)
			rep.StepFinished(ctx, scan.StepSplat, scan.StepFailed, "メモリ不足の可能性があります。")
			rep.StepFinished(ctx, scan.StepSOG, scan.StepSkipped, "")
			return nil
		}))
		id, ws := e.enqueue()
		e.start()
		d := e.waitStatus(id, scan.StatusPartial)
		if d.Scan.ErrorCode != "splat_failed" || d.Scan.ErrorMessage == "" {
			t.Errorf("スキャン = %+v", d.Scan)
		}
		if splat := d.Steps[3]; splat.Status != scan.StepFailed || splat.ErrorMessage != "メモリ不足の可能性があります。" || splat.Message != "" {
			t.Errorf("3DGS ステップ = %+v", splat)
		}
		if !exists(filepath.Join(ws.OutputDir(), "mesh.glb")) {
			t.Error("成功した成果物が消された")
		}
	})

	t.Run("SfM 失敗 → failed", func(t *testing.T) {
		e := newEnv(t, pipelineFunc(func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
			rep.StepStarted(ctx, scan.StepSfM)
			rep.StepFinished(ctx, scan.StepSfM, scan.StepFailed, "写真どうしの対応が見つかりません。")
			for _, s := range []scan.StepName{scan.StepMVS, scan.StepMesh, scan.StepSplat, scan.StepSOG} {
				rep.StepFinished(ctx, s, scan.StepSkipped, "SfM が失敗したため実行していません。")
			}
			return nil
		}))
		id, _ := e.enqueue()
		e.start()
		d := e.waitStatus(id, scan.StatusFailed)
		if d.Scan.ErrorCode != "sfm_failed" || d.Scan.ErrorMessage != "写真どうしの対応が見つかりません。" {
			t.Errorf("スキャン = %+v", d.Scan)
		}
	})

	t.Run("何も報告せずに終わるパイプラインは成功にならない", func(t *testing.T) {
		// 偽物や不具合で「何もしないで正常終了」しても、成果物が無ければ failed になる。
		e := newEnv(t, pipelineFunc(func(context.Context, reconstruction.Job, reconstruction.Reporter) error { return nil }))
		id, _ := e.enqueue()
		e.start()
		if d := e.waitStatus(id, scan.StatusFailed); d.Scan.ErrorCode != "no_output" {
			t.Errorf("スキャン = %+v", d.Scan)
		}
	})
}

// blockingPipeline は「始まったことを知らせ、ctx が取り消されるまで待つ」パイプライン。
func blockingPipeline(started chan<- string) pipelineFunc {
	return func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		rep.StepStarted(ctx, scan.StepSfM)
		// 途中経過のファイルを作っておく（キャンセル後に消えることを確かめるため）。
		os.WriteFile(job.WS.DatabasePath(), []byte("db"), 0o640)
		started <- job.ID
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestRemoveRunningJobStopsItAndDeletesFiles(t *testing.T) {
	started := make(chan string, 1)
	e := newEnv(t, blockingPipeline(started))
	id, ws := e.enqueue()
	e.start()
	<-started

	res, err := e.svc.Remove(context.Background(), id)
	if err != nil || res != RemovalCancelled {
		t.Fatalf("Remove = %q, %v", res, err)
	}
	d := e.waitStatus(id, scan.StatusCancelled)
	if d.Steps[0].Status != scan.StepSkipped {
		t.Errorf("実行中だったステップ = %q, want skipped", d.Steps[0].Status)
	}

	// ファイルはワーカーが外部プロセスの停止後に消す。少し待って確かめる。
	deadline := time.Now().Add(5 * time.Second)
	for exists(ws.Dir()) {
		if time.Now().After(deadline) {
			t.Fatal("キャンセル後も作業ディレクトリが残っている")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 止まった後で状態が書き換わらない（成功や失敗で上書きされない）。
	time.Sleep(30 * time.Millisecond)
	if got := e.get(id).Scan.Status; got != scan.StatusCancelled {
		t.Errorf("キャンセル後の状態が %q に変わった", got)
	}
}

func TestRemoveQueuedJobNeverRuns(t *testing.T) {
	var ran atomic.Int32
	e := newEnv(t, pipelineFunc(func(context.Context, reconstruction.Job, reconstruction.Reporter) error {
		ran.Add(1)
		return nil
	}))
	id, ws := e.enqueue()

	// ワーカーを起動する前（＝まだ queued）に消す。
	res, err := e.svc.Remove(context.Background(), id)
	if err != nil || res != RemovalCancelled {
		t.Fatalf("Remove = %q, %v", res, err)
	}
	if exists(ws.Dir()) {
		t.Error("queued のキャンセルで作業ディレクトリが消えていない")
	}
	e.start()
	time.Sleep(50 * time.Millisecond)
	if ran.Load() != 0 {
		t.Error("キャンセル済みのジョブが実行された")
	}
	if got := e.get(id).Scan.Status; got != scan.StatusCancelled {
		t.Errorf("状態 = %q", got)
	}
}

func TestRemoveFinishedJobDeletesRowAndFiles(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	id, ws := e.enqueue()
	e.start()
	e.waitStatus(id, scan.StatusSucceeded)

	res, err := e.svc.Remove(context.Background(), id)
	if err != nil || res != RemovalDeleted {
		t.Fatalf("Remove = %q, %v", res, err)
	}
	if exists(ws.Dir()) {
		t.Error("ファイルが残っている")
	}
	if _, err := e.store.Get(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("行が残っている: %v", err)
	}
	// 2回目は「無い」。
	if _, err := e.svc.Remove(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("2回目の Remove: %v", err)
	}
}

func TestRemoveRejectsUnknownOrMalformedID(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	unknown, _ := scan.NewID()
	for _, id := range []string{unknown, "../../etc", ""} {
		if _, err := e.svc.Remove(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Remove(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

// TestCompletionRacingWithCancel: パイプラインが最後まで走った直後にキャンセルが入った場合、
// 結果は cancelled のままで、ファイルも消える（成功で上書きされない）。
func TestCompletionRacingWithCancel(t *testing.T) {
	var e *env
	e = newEnv(t, pipelineFunc(func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		if err := succeedAll(t)(ctx, job, rep); err != nil {
			return err
		}
		// 「処理は終わったが、ワーカーが結果を記録する前」にキャンセルが DB に届いた状況を作る。
		_, _, err := e.store.Cancel(ctx, job.ID, e.clock.Now())
		return err
	}))
	id, ws := e.enqueue()
	e.start()
	e.waitStatus(id, scan.StatusCancelled)

	deadline := time.Now().Add(5 * time.Second)
	for exists(ws.Dir()) {
		if time.Now().After(deadline) {
			t.Fatal("キャンセルされたのに成果物が残っている")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.get(id).Scan.Status; got != scan.StatusCancelled {
		t.Errorf("状態 = %q, want cancelled", got)
	}
}

// TestShutdownThenRecoverRetries: サーバー停止で中断されたジョブは、次の起動時にやり直される。
func TestShutdownThenRecoverRetries(t *testing.T) {
	started := make(chan string, 1)
	var attempt atomic.Int32
	var e *env
	e = newEnv(t, pipelineFunc(func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		if attempt.Add(1) == 1 {
			return blockingPipeline(started)(ctx, job, rep) // 1回目は停止されるまで終わらない
		}
		// 2回目: 前回の途中経過が消えていること、写真は残っていることを確かめてから成功する。
		if exists(job.WS.DatabasePath()) {
			t.Error("やり直しなのに前回の database.db が残っている")
		}
		if !exists(filepath.Join(job.WS.ImagesDir(), "frame_0001.jpg")) {
			t.Error("やり直しで写真まで消えた")
		}
		return succeedAll(t)(ctx, job, rep)
	}))
	id, ws := e.enqueue()

	stop := e.start()
	<-started
	stop() // サーバー停止（SIGTERM 相当）

	// 停止では状態を書き換えない。running のまま残り、ファイルも残る。
	if d := e.get(id); d.Scan.Status != scan.StatusRunning {
		t.Fatalf("停止後の状態 = %q, want running", d.Scan.Status)
	}
	if !exists(ws.DatabasePath()) {
		t.Fatal("停止で途中経過が消えた（消すのはやり直しの直前）")
	}

	// 次の起動: 回復 → ワーカー開始。
	if err := e.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := e.get(id); d.Scan.Status != scan.StatusQueued {
		t.Fatalf("回復後の状態 = %q, want queued", d.Scan.Status)
	}
	e.start()
	d := e.waitStatus(id, scan.StatusSucceeded)
	if d.Scan.Attempts != 2 {
		t.Errorf("着手回数 = %d, want 2", d.Scan.Attempts)
	}
}

// TestRecoverGivesUpAfterMaxAttempts: 中断を繰り返すジョブは、上限に達したら失敗にする。
func TestRecoverGivesUpAfterMaxAttempts(t *testing.T) {
	started := make(chan string, 1)
	e := newEnv(t, blockingPipeline(started))
	id, _ := e.enqueue()

	for i := 0; i < 2; i++ { // MaxAttempts = 2
		stop := e.start()
		<-started
		stop()
		if err := e.svc.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	d := e.get(id)
	if d.Scan.Status != scan.StatusFailed || d.Scan.ErrorCode != "interrupted" || d.Scan.ErrorMessage == "" {
		t.Fatalf("上限到達後 = %+v", d.Scan)
	}
}

// TestWorkerRunsOneJobAtATimeInOrder: 同時実行は1件だけ。古い順に処理する。
func TestWorkerRunsOneJobAtATimeInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var active, maxActive int
	e := newEnv(t, pipelineFunc(func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		mu.Lock()
		active++
		maxActive = max(maxActive, active)
		order = append(order, job.ID)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return succeedAll(t)(ctx, job, rep)
	}))
	var ids []string
	for i := 0; i < 3; i++ {
		id, _ := e.enqueue()
		ids = append(ids, id)
	}
	e.start()
	for _, id := range ids {
		e.waitStatus(id, scan.StatusSucceeded)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Errorf("同時に %d 件が実行された（1件のはず）", maxActive)
	}
	for i := range ids {
		if order[i] != ids[i] {
			t.Fatalf("処理順 = %v, want %v", order, ids)
		}
	}
}

func TestSweepExpiresOldScans(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	oldID, oldWS := e.enqueue()
	e.start()
	e.waitStatus(oldID, scan.StatusSucceeded)

	e.clock.Advance(23 * time.Hour)
	freshID, freshWS := e.enqueue()
	e.waitStatus(freshID, scan.StatusSucceeded)

	// 期限（24時間）の手前では何も消さない。
	if res, err := e.svc.Sweep(context.Background()); err != nil || res != (SweepResult{}) {
		t.Fatalf("期限前の Sweep = %+v, %v", res, err)
	}

	e.clock.Advance(2 * time.Hour) // old は 25時間経過、fresh は 2時間
	res, err := e.svc.Sweep(context.Background())
	if err != nil || res.Expired != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	if exists(oldWS.Dir()) {
		t.Error("期限切れのファイルが残っている")
	}
	d := e.get(oldID)
	if d.Scan.Status != scan.StatusExpired || len(d.Assets) != 0 {
		t.Errorf("期限切れ後 = %q / 成果物 %v", d.Scan.Status, d.Assets)
	}
	if !exists(freshWS.Dir()) || e.get(freshID).Scan.Status != scan.StatusSucceeded {
		t.Error("期限内のスキャンまで消えた")
	}

	// 「期限切れ」と表示するための行は、期限からさらに一定期間（7日）残してから消す。
	// 7日進めると: old は期限から7日+1時間 → 行も消える。fresh は期限切れになるが、行はまだ残る。
	e.clock.Advance(7 * 24 * time.Hour)
	res, err = e.svc.Sweep(context.Background())
	if err != nil || res.Purged != 1 || res.Expired != 1 {
		t.Fatalf("行の削除 = %+v, %v", res, err)
	}
	if _, err := e.store.Get(context.Background(), oldID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("期限切れの行が残っている: %v", err)
	}
	if got := e.get(freshID).Scan.Status; got != scan.StatusExpired {
		t.Errorf("fresh の状態 = %q, want expired（行はまだ残る）", got)
	}
}

// TestSweepStopsExpiredRunningJob: 期限を過ぎても走っているジョブは止め、次の掃除で期限切れにする。
func TestSweepStopsExpiredRunningJob(t *testing.T) {
	started := make(chan string, 1)
	e := newEnv(t, blockingPipeline(started))
	id, ws := e.enqueue()
	e.start()
	<-started

	e.clock.Advance(25 * time.Hour)
	if _, err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(id, scan.StatusCancelled)
	deadline := time.Now().Add(5 * time.Second)
	for exists(ws.Dir()) {
		if time.Now().After(deadline) {
			t.Fatal("止めたジョブのファイルが残っている")
		}
		time.Sleep(5 * time.Millisecond)
	}
	res, err := e.svc.Sweep(context.Background())
	if err != nil || res.Expired != 1 {
		t.Fatalf("2回目の Sweep = %+v, %v", res, err)
	}
	if got := e.get(id).Scan.Status; got != scan.StatusExpired {
		t.Errorf("状態 = %q, want expired", got)
	}
}

// TestSweepRemovesOnlyOldOrphans: DB に行の無いディレクトリは、十分古いものだけ消す。
func TestSweepRemovesOnlyOldOrphans(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	makeOrphan := func(age time.Duration) *workspace.Scan {
		id, _ := scan.NewID()
		ws, _ := e.root.Scan(id)
		if err := ws.Create(); err != nil {
			t.Fatal(err)
		}
		// ディレクトリの更新時刻を「age だけ前」にする（時計は Service 側の偽時計が基準）。
		at := e.clock.Now().Add(-age)
		if err := os.Chtimes(ws.Dir(), at, at); err != nil {
			t.Fatal(err)
		}
		return ws
	}
	uploading := makeOrphan(5 * time.Minute) // 受付の途中かもしれない
	abandoned := makeOrphan(3 * time.Hour)   // 受付の途中で落ちた残骸

	res, err := e.svc.Sweep(context.Background())
	if err != nil || res.Orphans != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	if !exists(uploading.Dir()) {
		t.Error("受付中かもしれない新しいディレクトリを消した")
	}
	if exists(abandoned.Dir()) {
		t.Error("古い孤児ディレクトリが残っている")
	}
}

// TestSweepKeepsSavedOrphans: DB に行が無くても、保存したスキャンの目印があるディレクトリは消さない
// （DB を作り直した・古いバックアップから戻した場合に、保存した 3D データを失わないため）。
func TestSweepKeepsSavedOrphans(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	id, _ := scan.NewID()
	ws, _ := e.root.Scan(id)
	if err := ws.Create(); err != nil {
		t.Fatal(err)
	}
	if err := ws.MarkSaved(); err != nil {
		t.Fatal(err)
	}
	at := e.clock.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(ws.Dir(), at, at); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Sweep(context.Background())
	if err != nil || res.Orphans != 0 || res.KeptOrphans != 1 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	if !exists(ws.Dir()) {
		t.Fatal("保存したスキャンのディレクトリを消した")
	}
}

// TestRecoverMarksSavedScans: 目印の仕組みより前に保存したスキャンにも、起動時に目印を置く。
func TestRecoverMarksSavedScans(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	e.svc.cfg.KeepCompleted = true
	stop := e.start()
	id, ws := e.enqueue()
	e.svc.Notify()
	e.waitStatus(id, scan.StatusSucceeded)
	stop()
	if err := os.Remove(filepath.Join(ws.Dir(), "saved")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ws.IsSaved() {
		t.Fatal("起動時に保存済みのスキャンへ目印を置いていない")
	}
}

// TestGalleryKeepsCompletedScans: KeepCompleted のとき、完成したものはギャラリーに保存され、
// 中間ファイル（dense/）だけが消え、保存期限を過ぎても消されない。失敗したものはこれまでどおり消える。
func TestGalleryKeepsCompletedScans(t *testing.T) {
	var failing atomic.Bool
	e := newEnv(t, pipelineFunc(func(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
		// 中間ファイルの代わり。保存時に消えることを確かめる。
		os.MkdirAll(filepath.Join(job.WS.DenseDir(), "images"), 0o750)
		os.WriteFile(filepath.Join(job.WS.DenseDir(), "images", "frame_0001.jpg"), []byte("undistorted"), 0o640)
		if failing.Load() {
			rep.StepFinished(ctx, scan.StepSfM, scan.StepFailed, "写真どうしが重なっていません。")
			return nil
		}
		return succeedAll(t)(ctx, job, rep)
	}))
	e.svc.cfg.KeepCompleted = true
	e.start()

	keptID, keptWS := e.enqueue()
	e.svc.Notify()
	d := e.waitStatus(keptID, scan.StatusSucceeded)
	if !d.Scan.Saved {
		t.Fatal("完成したのに保存されていない")
	}
	if !keptWS.IsSaved() {
		t.Error("保存したスキャンに目印が無い（DB に行が無くなったとき掃除で消される）")
	}
	if exists(keptWS.DenseDir()) {
		t.Error("中間ファイル（dense/）が残っている")
	}
	for _, p := range []string{keptWS.ImagesDir(), filepath.Join(keptWS.OutputDir(), "mesh.glb")} {
		if !exists(p) {
			t.Errorf("%s が消えた（残すはず）", p)
		}
	}

	failing.Store(true)
	failedID, failedWS := e.enqueue()
	e.svc.Notify()
	if d := e.waitStatus(failedID, scan.StatusFailed); d.Scan.Saved {
		t.Error("失敗したものが保存された")
	}

	// 期限（24時間）を過ぎても、保存したものは消えない。失敗したものは消える。
	e.clock.Advance(25 * time.Hour)
	res, err := e.svc.Sweep(context.Background())
	if err != nil || res.Expired != 1 {
		t.Fatalf("Sweep = %+v, %v（失敗した1件だけが期限切れのはず）", res, err)
	}
	if got := e.get(keptID).Scan.Status; got != scan.StatusSucceeded || !exists(keptWS.Dir()) {
		t.Errorf("保存したものが消えた: status=%q", got)
	}
	if exists(failedWS.Dir()) || e.get(failedID).Scan.Status != scan.StatusExpired {
		t.Error("失敗したものが期限で消えていない")
	}
}

// TestGalleryOffKeepsOldBehaviour: KeepCompleted=false なら、完成したものも期限で消える（従来どおり）。
func TestGalleryOffKeepsOldBehaviour(t *testing.T) {
	e := newEnv(t, succeedAll(t))
	id, _ := e.enqueue()
	e.start()
	if d := e.waitStatus(id, scan.StatusSucceeded); d.Scan.Saved {
		t.Fatal("KeepCompleted=false なのに保存された")
	}
}
