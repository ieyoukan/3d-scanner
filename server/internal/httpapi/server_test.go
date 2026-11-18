package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/jobs"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/vps"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// このテストは、本物のハンドラ・本物のジョブ管理・メモリ上の Store・偽のパイプラインを組み合わせ、
// HTTP の入口から「アップロード → 待ち行列 → 処理 → 状態取得 → 成果物配信」までを通しで確かめる。

// fakePipeline は全ステップ成功として成果物ファイルを置く。
type fakePipeline struct{}

func (fakePipeline) Run(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
	write := func(kind scan.AssetKind, name, content string) {
		path := filepath.Join(job.WS.OutputDir(), name)
		os.WriteFile(path, []byte(content), 0o640)
		rep.AssetReady(ctx, kind, path, int64(len(content)))
	}
	rep.StepStarted(ctx, scan.StepSfM)
	os.WriteFile(job.WS.LogPath(scan.StepSfM), []byte("$ colmap feature_extractor\n特徴点を抽出しました\n"), 0o640)
	// SfM の結果（地図）。これがあると「続きを撮る」と位置合わせの対象になる。
	os.MkdirAll(job.WS.ModelDir(), 0o750)
	os.WriteFile(job.WS.DatabasePath(), []byte("sqlite"), 0o640)
	os.WriteFile(filepath.Join(job.WS.ModelDir(), "images.bin"), []byte("bin"), 0o640)
	write(scan.AssetSparsePLY, "sparse.ply", "ply\nsparse")
	rep.StepFinished(ctx, scan.StepSfM, scan.StepSucceeded, "3枚中3枚")
	rep.StepFinished(ctx, scan.StepMVS, scan.StepSkipped, "GPU なし")
	rep.StepStarted(ctx, scan.StepMesh)
	write(scan.AssetMeshGLB, "mesh.glb", "glTF-0123456789")
	rep.StepFinished(ctx, scan.StepMesh, scan.StepSucceeded, "")
	if job.DeferSplat {
		return nil // 本物と同じく、メッシュが出来たら 3DGS は後回しにする
	}
	rep.StepStarted(ctx, scan.StepSplat)
	rep.StepFinished(ctx, scan.StepSplat, scan.StepFailed, "メモリ不足の可能性があります。")
	rep.StepFinished(ctx, scan.StepSOG, scan.StepSkipped, "")
	return nil
}

// RunSplat は 3DGS の学習と圧縮が成功したことにする。
func (fakePipeline) RunSplat(ctx context.Context, job reconstruction.Job, rep reconstruction.Reporter) error {
	for _, a := range []struct {
		kind scan.AssetKind
		name string
		data string
	}{{scan.AssetGaussianPLY, "splat.ply", "ply\nsplat"}, {scan.AssetGaussianSOG, "scene.sog", "PK-sog"}} {
		path := filepath.Join(job.WS.OutputDir(), a.name)
		os.WriteFile(path, []byte(a.data), 0o640)
		rep.AssetReady(ctx, a.kind, path, int64(len(a.data)))
	}
	rep.StepStarted(ctx, scan.StepSplat)
	rep.StepFinished(ctx, scan.StepSplat, scan.StepSucceeded, "")
	rep.StepStarted(ctx, scan.StepSOG)
	rep.StepFinished(ctx, scan.StepSOG, scan.StepSucceeded, "")
	return nil
}

// testEnv はテスト1件ぶんのサーバー一式。
type testEnv struct {
	t      *testing.T
	cfg    config.Config
	store  *store.Memory
	root   *workspace.Root
	jobs   *jobs.Service
	server *Server
	http   http.Handler
}

// newTestEnv はサーバーを組み立てる。mutate で設定を変えられる。
func newTestEnv(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	webDir := t.TempDir()
	os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<!doctype html><title>3D Scanner</title>"), 0o644)
	os.MkdirAll(filepath.Join(webDir, "assets"), 0o755)
	os.WriteFile(filepath.Join(webDir, "assets", "index-abc123.js"), []byte("console.log('app')"), 0o644)

	cfg := config.Config{
		DataDir: t.TempDir(),
		WebDir:  webDir,
		Limits: config.Limits{
			MinImages: 3, MaxImages: 6, MaxImageBytes: 200 * 1024, MaxUploadBytes: 600 * 1024,
			MaxImagePixels: 1_000_000, MinImageSide: 32, MinFreeDiskBytes: 1, MaxQueuedScans: 2,
		},
		Jobs: config.Jobs{Retention: 24 * time.Hour, ExpiredRowRetention: time.Hour, MaxAttempts: 2,
			PollInterval: 10 * time.Millisecond, JanitorInterval: time.Hour},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	root, err := workspace.New(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	js := jobs.New(st, root, fakePipeline{}, cfg.Jobs, nil)
	srv := New(cfg, st, root, js, &fakeLocalizer{}, PipelineInfo{MeshEnabled: true, MeshMode: "sparse", SplatEnabled: true, SplatDevice: "cpu"}, nil)
	return &testEnv{t: t, cfg: cfg, store: st, root: root, jobs: js, server: srv, http: srv.Handler()}
}

// startWorker はワーカーを動かす（テスト終了時に止める）。
func (e *testEnv) startWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.jobs.Run(ctx); close(done) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

// do はリクエストを1つ実行する。opts でヘッダ等を足せる。
func (e *testEnv) do(method, target string, body io.Reader, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	e.http.ServeHTTP(rec, req)
	return rec
}

func header(k, v string) func(*http.Request) {
	return func(req *http.Request) { req.Header.Set(k, v) }
}

// photo は seed ごとに中身の違う小さな JPEG を作る。
func photo(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x) * seed, G: uint8(y) + seed, B: seed, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uploadBody は n 枚の写真を入れた multipart ボディと Content-Type を返す。
func uploadBody(t *testing.T, n int) (io.Reader, func(*http.Request)) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i < n; i++ {
		fw, _ := w.CreateFormFile("images", fmt.Sprintf("IMG_%d.jpg", i))
		fw.Write(photo(t, uint8(i+1)))
	}
	w.WriteField("metadata", `{"client":{"userAgent":"go-test"},"frames":[{"capturedAt":"2026-11-16T09:00:00Z"}]}`)
	w.Close()
	return &body, header("Content-Type", w.FormDataContentType())
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("JSON として読めない（status %d）: %s", rec.Code, rec.Body.String())
	}
	return v
}

// wantError はエラー応答の status と code を確かめる。
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d（body: %s）", rec.Code, status, rec.Body.String())
	}
	body := decode[errorBody](t, rec)
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("error = %+v, want code %q と説明文", body.Error, code)
	}
}

// createScan はアップロードして ID を返す。
func (e *testEnv) createScan(n int) string {
	e.t.Helper()
	body, ct := uploadBody(e.t, n)
	rec := e.do("POST", "/api/scans", body, ct)
	if rec.Code != http.StatusAccepted {
		e.t.Fatalf("アップロードが %d: %s", rec.Code, rec.Body.String())
	}
	return decode[createResponse](e.t, rec).ID
}

// waitScan は状態が終了状態になるまで GET を繰り返す（画面のポーリングと同じ）。
func (e *testEnv) waitScan(id string) scanResponse {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := e.do("GET", "/api/scans/"+id, nil)
		resp := decode[scanResponse](e.t, rec)
		if resp.Status.Terminal() {
			return resp
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("終了しない（現在 %q）", resp.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *testEnv) scanDirs() []string {
	ids, _ := e.root.List()
	return ids
}

// ---------------------------------------------------------------------------

func TestHealthAndConfig(t *testing.T) {
	e := newTestEnv(t, nil)
	if rec := e.do("GET", "/healthz", nil); rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/readyz", nil); rec.Code != 200 {
		t.Errorf("readyz = %d %s", rec.Code, rec.Body.String())
	}

	// 設定は画面が最初に読む。秘密は含まれない。
	cfg := decode[configResponse](t, e.do("GET", "/api/config", nil))
	if cfg.Limits.MinImages != 3 || cfg.Limits.MaxImages != 6 || cfg.RetentionHours != 24 || cfg.Pipeline.MeshMode != "sparse" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newTestEnv(t, nil)
	h := e.do("GET", "/", nil).Header()
	for name, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Permissions-Policy":      "camera=(self)",
		"Content-Security-Policy": "default-src 'self'",
	} {
		if !strings.Contains(h.Get(name), want) {
			t.Errorf("%s = %q, want %q を含む", name, h.Get(name), want)
		}
	}
}

// TestCrossOriginWritesAreRejected: POST/DELETE は、自分のページ発でなければ拒否する（CSRF 対策）。
// アプリ自身は認証を持たないが、前段の Cloudflare Access の Cookie は別サイトのページ発のリクエストにも付き得る。
func TestCrossOriginWritesAreRejected(t *testing.T) {
	e := newTestEnv(t, nil)
	id := e.createScan(3)

	// 同じドメインの別サブドメイン（同一サイトだが別オリジン）からの削除。
	// httptest のリクエストの Host は example.com。
	rec := e.do("DELETE", "/api/scans/"+id, nil, header("Origin", "https://other.example.com"))
	wantError(t, rec, http.StatusForbidden, "cross_origin")
	if rec := e.do("GET", "/api/scans/"+id, nil); rec.Code != 200 {
		t.Fatal("拒否したはずの削除が実行された")
	}
	upload, ct := uploadBody(t, 3)
	wantError(t, e.do("POST", "/api/scans", upload, ct, header("Origin", "https://evil.test")), http.StatusForbidden, "cross_origin")

	// 自分のオリジンからなら通る。
	if rec := e.do("DELETE", "/api/scans/"+id, nil, header("Origin", "https://example.com")); rec.Code != 200 {
		t.Errorf("同一オリジンの削除が %d", rec.Code)
	}
	// Origin が無いのはブラウザ以外（curl 等）。通る。
	id2 := e.createScan(3)
	if rec := e.do("DELETE", "/api/scans/"+id2, nil); rec.Code != 200 {
		t.Errorf("Origin 無しの削除が %d", rec.Code)
	}
}

// TestNoSessionEndpoint: ログインの仕組みは無い。
func TestNoSessionEndpoint(t *testing.T) {
	e := newTestEnv(t, nil)
	wantError(t, e.do("POST", "/api/session", nil), http.StatusNotFound, "not_found")
}

// TestScanLifecycle: アップロードから成果物の取得までの一連の流れ。
func TestScanLifecycle(t *testing.T) {
	e := newTestEnv(t, nil)

	body, ct := uploadBody(t, 4)
	rec := e.do("POST", "/api/scans", body, ct)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	created := decode[createResponse](t, rec)
	if !scan.ValidID(created.ID) || created.Status != scan.StatusQueued || created.FrameCount != 4 {
		t.Fatalf("応答 = %+v", created)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/scans/"+created.ID {
		t.Errorf("Location = %q", loc)
	}
	// 写真はサーバーが決めた名前で保存されている（利用者のファイル名 IMG_0.jpg は使わない）。
	ws, _ := e.root.Scan(created.ID)
	entries, _ := os.ReadDir(ws.ImagesDir())
	if len(entries) != 4 || entries[0].Name() != "frame_0001.jpg" {
		t.Errorf("保存された写真 = %v", entries)
	}

	// ワーカーが動く前は queued で、待ち順が分かる。
	queued := decode[scanResponse](t, e.do("GET", "/api/scans/"+created.ID, nil))
	if queued.Status != scan.StatusQueued || queued.QueuePosition == nil || *queued.QueuePosition != 0 || len(queued.Steps) != 5 {
		t.Fatalf("queued の応答 = %+v", queued)
	}
	// 処理前は成果物を配信しない。
	wantError(t, e.do("GET", "/api/scans/"+created.ID+"/assets/mesh_glb", nil), http.StatusGone, "asset_unavailable")

	e.startWorker()
	e.jobs.Notify()
	done := e.waitScan(created.ID)

	// 偽パイプラインは「Mesh 成功・3DGS 失敗」を報告する → partial。失敗は隠されない。
	if done.Status != scan.StatusPartial || done.Error == nil || done.Error.Code != "splat_failed" {
		t.Fatalf("終了時 = %+v（error=%+v）", done.Status, done.Error)
	}
	if done.QueuePosition != nil {
		t.Error("終了後に待ち順が付いている")
	}
	byName := map[scan.StepName]stepResponse{}
	for _, st := range done.Steps {
		byName[st.Name] = st
	}
	if st := byName[scan.StepSfM]; st.Status != scan.StepSucceeded || st.Message != "3枚中3枚" || !st.HasLog {
		t.Errorf("SfM = %+v", st)
	}
	if st := byName[scan.StepSplat]; st.Status != scan.StepFailed || st.Error == "" {
		t.Errorf("3DGS = %+v", st)
	}
	kinds := map[scan.AssetKind]assetResponse{}
	for _, a := range done.Assets {
		kinds[a.Kind] = a
	}
	// URL の末尾には拡張子つきのファイル名が付く（ビューアが形式を見分けるため）。
	if len(kinds) != 2 || kinds[scan.AssetMeshGLB].URL != "/api/scans/"+created.ID+"/assets/mesh_glb/mesh.glb" {
		t.Fatalf("成果物 = %+v", done.Assets)
	}
	// ファイル名なしの URL（仕様どおりの形）でも同じものが取れる。
	if plain := e.do("GET", "/api/scans/"+created.ID+"/assets/mesh_glb", nil); plain.Code != 200 || plain.Body.String() != "glTF-0123456789" {
		t.Errorf("ファイル名なしの URL = %d %q", plain.Code, plain.Body.String())
	}
	// 種類と合わないファイル名は受け付けない（名前でファイルを選ばせない）。
	for _, name := range []string{"scene.sog", "..%2f..%2fsecret", "mesh.glb.bak"} {
		if rec := e.do("GET", "/api/scans/"+created.ID+"/assets/mesh_glb/"+name, nil); rec.Code == 200 {
			t.Errorf("名前 %q で成果物が取れてしまった", name)
		}
	}

	// 成果物の取得。
	asset := e.do("GET", kinds[scan.AssetMeshGLB].URL, nil)
	if asset.Code != 200 || asset.Body.String() != "glTF-0123456789" || asset.Header().Get("Content-Type") != "model/gltf-binary" {
		t.Errorf("成果物 = %d %q (%s)", asset.Code, asset.Body.String(), asset.Header().Get("Content-Type"))
	}
	if cc := asset.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("成果物の Cache-Control = %q（共有キャッシュに残さない）", cc)
	}
	if asset.Header().Get("Content-Disposition") != "" {
		t.Error("download を指定していないのに attachment が付いた")
	}
	// ?download=1 でファイルとして保存させる。
	dl := e.do("GET", kinds[scan.AssetMeshGLB].URL+"?download=1", nil)
	if cd := dl.Header().Get("Content-Disposition"); cd != "attachment; filename=mesh.glb" {
		t.Errorf("Content-Disposition = %q", cd)
	}
	// Range（途中からの取得）に対応している。
	part := e.do("GET", kinds[scan.AssetMeshGLB].URL, nil, header("Range", "bytes=0-3"))
	if part.Code != http.StatusPartialContent || part.Body.String() != "glTF" {
		t.Errorf("Range = %d %q", part.Code, part.Body.String())
	}
	// 生成に失敗した種類は 404（成功したふりをしない）。
	wantError(t, e.do("GET", "/api/scans/"+created.ID+"/assets/gaussian_ply", nil), http.StatusNotFound, "asset_not_ready")
	wantError(t, e.do("GET", "/api/scans/"+created.ID+"/assets/passwd", nil), http.StatusNotFound, "unknown_asset")

	// 処理ログ。
	logs := e.do("GET", "/api/scans/"+created.ID+"/logs?step=sfm", nil)
	if logs.Code != 200 || !strings.Contains(logs.Body.String(), "colmap feature_extractor") || !strings.HasPrefix(logs.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("ログ = %d %q", logs.Code, logs.Body.String())
	}
	wantError(t, e.do("GET", "/api/scans/"+created.ID+"/logs?step=splat", nil), http.StatusNotFound, "no_log")

	// 削除すると、行もファイルも無くなる。
	del := e.do("DELETE", "/api/scans/"+created.ID, nil)
	if del.Code != 200 || decode[map[string]string](t, del)["result"] != "deleted" {
		t.Errorf("削除 = %d %s", del.Code, del.Body.String())
	}
	wantError(t, e.do("GET", "/api/scans/"+created.ID, nil), http.StatusNotFound, "not_found")
	if dirs := e.scanDirs(); len(dirs) != 0 {
		t.Errorf("削除後も作業ディレクトリが残っている: %v", dirs)
	}
}

// TestUploadRejections: 断ったアップロードは、理由を返し、サーバーに何も残さない。
func TestUploadRejections(t *testing.T) {
	t.Run("multipart でない", func(t *testing.T) {
		e := newTestEnv(t, nil)
		rec := e.do("POST", "/api/scans", strings.NewReader(`{"images":[]}`), header("Content-Type", "application/json"))
		wantError(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
	})
	t.Run("枚数不足", func(t *testing.T) {
		e := newTestEnv(t, nil)
		body, ct := uploadBody(t, 2)
		wantError(t, e.do("POST", "/api/scans", body, ct), http.StatusBadRequest, "too_few_images")
		if dirs := e.scanDirs(); len(dirs) != 0 {
			t.Errorf("断ったのに作業ディレクトリが残っている: %v", dirs)
		}
		if n, _ := e.store.CountActive(context.Background()); n != 0 {
			t.Errorf("断ったのにジョブが登録された: %d", n)
		}
	})
	t.Run("枚数超過", func(t *testing.T) {
		e := newTestEnv(t, nil)
		body, ct := uploadBody(t, 7)
		wantError(t, e.do("POST", "/api/scans", body, ct), http.StatusRequestEntityTooLarge, "too_many_images")
		if dirs := e.scanDirs(); len(dirs) != 0 {
			t.Errorf("作業ディレクトリが残っている: %v", dirs)
		}
	})
	t.Run("全体が大きすぎる", func(t *testing.T) {
		e := newTestEnv(t, func(c *config.Config) { c.Limits.MaxUploadBytes = 2 * 1024 })
		body, ct := uploadBody(t, 4)
		wantError(t, e.do("POST", "/api/scans", body, ct), http.StatusRequestEntityTooLarge, "upload_too_large")
		if dirs := e.scanDirs(); len(dirs) != 0 {
			t.Errorf("作業ディレクトリが残っている: %v", dirs)
		}
	})
	t.Run("画像でないファイル", func(t *testing.T) {
		e := newTestEnv(t, nil)
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		for i := 0; i < 3; i++ {
			fw, _ := w.CreateFormFile("images", "x.jpg")
			fw.Write([]byte("#!/bin/sh\necho これは写真ではありません\n"))
		}
		w.Close()
		rec := e.do("POST", "/api/scans", &body, header("Content-Type", w.FormDataContentType()))
		wantError(t, rec, http.StatusUnsupportedMediaType, "unsupported_type")
	})
	t.Run("待ち行列が一杯", func(t *testing.T) {
		e := newTestEnv(t, nil) // MaxQueuedScans = 2、ワーカーは動かさない
		e.createScan(3)
		e.createScan(3)
		body, ct := uploadBody(t, 3)
		rec := e.do("POST", "/api/scans", body, ct)
		wantError(t, rec, http.StatusTooManyRequests, "queue_full")
		if rec.Header().Get("Retry-After") == "" {
			t.Error("Retry-After が無い")
		}
		if dirs := e.scanDirs(); len(dirs) != 2 {
			t.Errorf("作業ディレクトリ = %d 個, want 2", len(dirs))
		}
	})
	t.Run("ディスクの空きが足りない", func(t *testing.T) {
		e := newTestEnv(t, func(c *config.Config) { c.Limits.MinFreeDiskBytes = 1 << 60 }) // 1 EiB は必ず足りない
		body, ct := uploadBody(t, 3)
		wantError(t, e.do("POST", "/api/scans", body, ct), http.StatusInsufficientStorage, "disk_full")
	})
}

// TestMalformedIDsNeverReachTheFilesystem: URL の ID に何を入れても、ハンドラは何も返さず、何も実行しない。
//
// 期待する応答は2通り:
//   - 404: ハンドラが ID の形式検査で弾いた。
//   - 3xx: パスに生の ".." が含まれる場合。Go 標準の ServeMux は、ハンドラを呼ぶ前にパスを
//     正規化し、正規化後の URL へのリダイレクトを返す（".." を含むパスはハンドラに届かない）。
//
// どちらでも「不正な ID がファイルシステムに届かない」ことは満たされる。200 だけは決して返らない。
func TestMalformedIDsNeverReachTheFilesystem(t *testing.T) {
	e := newTestEnv(t, nil)
	// 実在するスキャンを1つ作っておく。不正な ID の操作がこれに影響しないことも確かめる。
	real := e.createScan(3)
	rejected := func(code int) bool {
		return code == http.StatusNotFound || (code >= 300 && code < 400)
	}
	unknown, _ := scan.NewID()
	for _, id := range []string{unknown, "abc", "..", "%2e%2e", "..%2f..%2fetc", "00000000-0000-4000-8000-00000000000Z"} {
		for _, suffix := range []string{"", "/logs?step=sfm", "/assets/mesh_glb"} {
			rec := e.do("GET", "/api/scans/"+id+suffix, nil)
			if !rejected(rec.Code) {
				t.Errorf("GET /api/scans/%s%s = %d（404 かリダイレクトのはず）", id, suffix, rec.Code)
			}
		}
		if rec := e.do("DELETE", "/api/scans/"+id, nil); !rejected(rec.Code) {
			t.Errorf("DELETE /api/scans/%s = %d（404 かリダイレクトのはず）", id, rec.Code)
		}
	}
	// 形式は正しいが存在しない ID と、明らかに不正な ID は、必ずハンドラ自身が 404 を返す。
	for _, id := range []string{unknown, "abc", "00000000-0000-4000-8000-00000000000Z"} {
		wantError(t, e.do("GET", "/api/scans/"+id, nil), http.StatusNotFound, "not_found")
		wantError(t, e.do("DELETE", "/api/scans/"+id, nil), http.StatusNotFound, "not_found")
	}
	// 実在するスキャンと、その写真は無傷。
	if rec := e.do("GET", "/api/scans/"+real, nil); rec.Code != http.StatusOK {
		t.Errorf("無関係なスキャンが影響を受けた: %d", rec.Code)
	}
	if dirs := e.scanDirs(); len(dirs) != 1 || dirs[0] != real {
		t.Errorf("作業ディレクトリ = %v, want [%s]", dirs, real)
	}
}

func TestLogsRejectUnknownStep(t *testing.T) {
	e := newTestEnv(t, nil)
	id := e.createScan(3)
	for _, step := range []string{"", "unknown", "../../../etc/passwd", "sfm/../../x"} {
		rec := e.do("GET", "/api/scans/"+id+"/logs?step="+step, nil)
		wantError(t, rec, http.StatusBadRequest, "bad_step")
	}
}

// TestAssetPathEscapeIsNotServed: 万一 DB の相対パスが作業ディレクトリの外を指していても、配信しない。
func TestAssetPathEscapeIsNotServed(t *testing.T) {
	e := newTestEnv(t, nil)
	id := e.createScan(3)
	e.startWorker()
	e.waitScan(id)

	secret := filepath.Join(e.cfg.DataDir, "secret.txt")
	os.WriteFile(secret, []byte("サーバー上の秘密"), 0o600)
	ctx := context.Background()

	// (1) ".." で外に出る相対パス。
	e.store.PutAsset(ctx, id, scan.Asset{Kind: scan.AssetGaussianPLY, RelativePath: "../../secret.txt", SizeBytes: 1})
	rec := e.do("GET", "/api/scans/"+id+"/assets/gaussian_ply", nil)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "秘密") {
		t.Fatalf("外を指すパスが配信された: %d %q", rec.Code, rec.Body.String())
	}
	// (2) 絶対パス。
	e.store.PutAsset(ctx, id, scan.Asset{Kind: scan.AssetGaussianPLY, RelativePath: secret, SizeBytes: 1})
	if rec := e.do("GET", "/api/scans/"+id+"/assets/gaussian_ply", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("絶対パスが配信された: %d", rec.Code)
	}
	// (3) 作業ディレクトリ内のシンボリックリンクが外を指している。
	ws, _ := e.root.Scan(id)
	link := filepath.Join(ws.OutputDir(), "link.ply")
	if err := os.Symlink(secret, link); err == nil {
		e.store.PutAsset(ctx, id, scan.Asset{Kind: scan.AssetGaussianPLY, RelativePath: "output/link.ply", SizeBytes: 1})
		rec := e.do("GET", "/api/scans/"+id+"/assets/gaussian_ply", nil)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "秘密") {
			t.Fatalf("外を指すリンクが配信された: %d %q", rec.Code, rec.Body.String())
		}
	}
}

func TestDeleteQueuedScanCancels(t *testing.T) {
	e := newTestEnv(t, nil)
	id := e.createScan(3)
	rec := e.do("DELETE", "/api/scans/"+id, nil)
	if rec.Code != 200 || decode[map[string]string](t, rec)["result"] != "cancelled" {
		t.Fatalf("削除 = %d %s", rec.Code, rec.Body.String())
	}
	got := decode[scanResponse](t, e.do("GET", "/api/scans/"+id, nil))
	if got.Status != scan.StatusCancelled {
		t.Errorf("状態 = %q", got.Status)
	}
	// 中止したスキャンの写真は残さない。成果物も配信しない。
	if dirs := e.scanDirs(); len(dirs) != 0 {
		t.Errorf("中止後も写真が残っている: %v", dirs)
	}
	wantError(t, e.do("GET", "/api/scans/"+id+"/assets/mesh_glb", nil), http.StatusGone, "asset_unavailable")
}

// TestStaticAndSPAFallback: Web の配信と、SPA のための index.html へのフォールバック。
func TestStaticAndSPAFallback(t *testing.T) {
	e := newTestEnv(t, nil)
	id, _ := scan.NewID()

	for _, path := range []string{"/", "/scans/" + id, "/scans/" + id + "/view"} {
		rec := e.do("GET", path, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>3D Scanner</title>") {
			t.Errorf("GET %s = %d %q（index.html のはず）", path, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s の Cache-Control = %q", path, cc)
		}
	}

	js := e.do("GET", "/assets/index-abc123.js", nil)
	if js.Code != 200 || js.Body.String() != "console.log('app')" || !strings.Contains(js.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("JS = %d %q cache=%q", js.Code, js.Body.String(), js.Header().Get("Cache-Control"))
	}
	// 存在しない JS に index.html を返さない（HTML を JS として読ませない）。
	if rec := e.do("GET", "/assets/missing.js", nil); rec.Code != http.StatusNotFound {
		t.Errorf("存在しない JS = %d", rec.Code)
	}
	// 存在しない API は JSON の 404（HTML を返さない）。
	wantError(t, e.do("GET", "/api/nope", nil), http.StatusNotFound, "not_found")

	// Web のディレクトリの外にあるファイルは、どう書いても取れない。
	os.WriteFile(filepath.Join(filepath.Dir(e.cfg.WebDir), "outside.txt"), []byte("外のファイル"), 0o644)
	for _, path := range []string{"/../outside.txt", "/assets/../../outside.txt", "/%2e%2e/outside.txt"} {
		rec := e.do("GET", path, nil)
		if strings.Contains(rec.Body.String(), "外のファイル") {
			t.Errorf("GET %s で Web ディレクトリの外が読めた", path)
		}
	}

	if rec := e.do("POST", "/", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d", rec.Code)
	}
}

// TestPanicDoesNotKillServer: ハンドラの不具合（パニック）は 500 になるだけで、プロセスは落ちない。
func TestPanicDoesNotKillServer(t *testing.T) {
	e := newTestEnv(t, nil)
	h := e.server.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("バグ") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	wantError(t, rec, http.StatusInternalServerError, "internal")
}

// TestGalleryListAndThumbnail: 一覧は新しい順で、続きを読め、サムネイルが付く。
func TestGalleryListAndThumbnail(t *testing.T) {
	// ワーカーを動かさないので3件とも queued のまま残る。受付の上限（既定 2）を上げておく。
	e := newTestEnv(t, func(c *config.Config) { c.Limits.MaxQueuedScans = 5 })
	var ids []string
	for range 3 {
		ids = append(ids, e.createScan(3))
		time.Sleep(2 * time.Millisecond) // 作成時刻をずらして並び順を決める
	}

	page := decode[galleryResponse](t, e.do("GET", "/api/scans?limit=2", nil))
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("1ページ目 = %d件, next=%q", len(page.Items), page.NextCursor)
	}
	if page.Items[0].ID != ids[2] || page.Items[1].ID != ids[1] {
		t.Fatalf("新しい順になっていない: %s, %s", page.Items[0].ID, page.Items[1].ID)
	}
	item := page.Items[0]
	if item.ThumbnailURL != "/api/scans/"+ids[2]+"/thumbnail" || item.FrameCount != 3 || item.AssetKinds == nil {
		t.Fatalf("項目 = %+v", item)
	}
	rest := decode[galleryResponse](t, e.do("GET", "/api/scans?limit=2&after="+page.NextCursor, nil))
	if len(rest.Items) != 1 || rest.Items[0].ID != ids[0] || rest.NextCursor != "" {
		t.Fatalf("2ページ目 = %+v", rest)
	}

	// サムネイルは長辺 480 以下の JPEG。
	rec := e.do("GET", item.ThumbnailURL, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumbnail = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	cfg, format, err := image.DecodeConfig(rec.Body)
	if err != nil || format != "jpeg" || max(cfg.Width, cfg.Height) > thumbnailSide {
		t.Fatalf("thumbnail = %s %dx%d err=%v", format, cfg.Width, cfg.Height, err)
	}
	// 個別の取得にも名前・保存状態・サムネイルが入る。
	if got := decode[scanResponse](t, e.do("GET", "/api/scans/"+ids[2], nil)); got.ThumbnailURL == "" || got.Saved {
		t.Fatalf("scan = %+v（まだ完成していないので saved=false）", got)
	}

	wantError(t, e.do("GET", "/api/scans?limit=0", nil), http.StatusBadRequest, "bad_limit")
	wantError(t, e.do("GET", "/api/scans?limit=1000", nil), http.StatusBadRequest, "bad_limit")
	wantError(t, e.do("GET", "/api/scans?after=../../etc", nil), http.StatusBadRequest, "bad_cursor")

	// 中止したものは写真が消えるので、サムネイルも出さない。
	e.do("DELETE", "/api/scans/"+ids[0], nil)
	wantError(t, e.do("GET", "/api/scans/"+ids[0]+"/thumbnail", nil), http.StatusGone, "thumbnail_unavailable")
}

func TestRenameScan(t *testing.T) {
	e := newTestEnv(t, nil)
	id := e.createScan(3)
	rename := func(body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
		return e.do("PATCH", "/api/scans/"+id, strings.NewReader(body), opts...)
	}
	if rec := rename(`{"title":"  机の上のマグカップ  "}`); rec.Code != 200 {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body.String())
	}
	if got := decode[scanResponse](t, e.do("GET", "/api/scans/"+id, nil)).Title; got != "机の上のマグカップ" {
		t.Fatalf("Title = %q（前後の空白は除く）", got)
	}
	wantError(t, rename(`{"title":"`+strings.Repeat("あ", 81)+`"}`), http.StatusBadRequest, "bad_title")
	wantError(t, rename(`{"title":"改行\n入り"}`), http.StatusBadRequest, "bad_title")
	wantError(t, rename(`{}`), http.StatusBadRequest, "bad_request")
	wantError(t, rename(`{"title":"x"}`, header("Origin", "https://evil.test")), http.StatusForbidden, "cross_origin")
	missing, _ := scan.NewID()
	wantError(t, e.do("PATCH", "/api/scans/"+missing, strings.NewReader(`{"title":"x"}`)), http.StatusNotFound, "not_found")
	// 81文字ちょうどではなく 80 文字は通る。
	if rec := rename(`{"title":"` + strings.Repeat("あ", 80) + `"}`); rec.Code != 200 {
		t.Fatalf("80文字の名前が拒否された: %d", rec.Code)
	}
}

// fakeLocalizer は位置合わせの偽物。決めた結果かエラーを返す。
type fakeLocalizer struct {
	result vps.Result
	err    error
	// got は渡された写真（呼ばれた時点でファイルがあったか）。
	got       vps.Query
	fileExist bool
}

func (f *fakeLocalizer) Localize(_ context.Context, m *workspace.Scan, q vps.Query) (vps.Result, error) {
	f.got = q
	_, err := os.Stat(q.Path)
	f.fileExist = err == nil
	return f.result, f.err
}

// resumeBody は n 枚の写真を入れた「続きを撮る」のボディ（seed をずらして元の写真と別の中身にする）。
func resumeBody(t *testing.T, n int) (io.Reader, func(*http.Request)) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i < n; i++ {
		fw, _ := w.CreateFormFile("images", fmt.Sprintf("IMG_%d.jpg", i))
		fw.Write(photo(t, uint8(100+i)))
	}
	w.Close()
	return &body, header("Content-Type", w.FormDataContentType())
}

func TestResumeScan(t *testing.T) {
	e := newTestEnv(t, nil)
	e.startWorker()
	parentID := e.createScan(3)
	parent := e.waitScan(parentID)
	if !parent.Resumable || len(parent.Continuations) != 0 || parent.ParentID != "" {
		t.Fatalf("元の版 = %+v", parent)
	}

	// 上限は元の写真と合わせて 6 枚。元が 3 枚なので、4 枚は多すぎる。
	body, ct := resumeBody(t, 4)
	wantError(t, e.do("POST", "/api/scans/"+parentID+"/resume", body, ct), http.StatusRequestEntityTooLarge, "too_many_images")

	// 1 枚から受け付ける。
	body, ct = resumeBody(t, 2)
	rec := e.do("POST", "/api/scans/"+parentID+"/resume", body, ct)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body.String())
	}
	created := decode[createResponse](t, rec)
	if created.FrameCount != 5 || created.BaseFrameCount != 3 {
		t.Fatalf("受付 = %+v", created)
	}

	// 続きの版の作業ディレクトリ: 引き継いだ写真3枚＋今回の2枚（続きの番号）と、元の版の地図。
	child, _ := e.root.Scan(created.ID)
	var names []string
	entries, _ := os.ReadDir(child.ImagesDir())
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if strings.Join(names, ",") != "frame_0001.jpg,frame_0002.jpg,frame_0003.jpg,frame_0004.jpg,frame_0005.jpg" {
		t.Fatalf("写真 = %v", names)
	}

	got := e.waitScan(created.ID)
	if got.ParentID != parentID || got.BaseFrameCount != 3 || got.FrameCount != 5 || !got.Resumable {
		t.Fatalf("続きの版 = %+v", got)
	}
	if after := decode[scanResponse](t, e.do("GET", "/api/scans/"+parentID, nil)); len(after.Continuations) != 1 ||
		after.Continuations[0].ID != created.ID {
		t.Fatalf("元の版の continuations = %+v", after.Continuations)
	}
	// 続きの版が完成したので、一覧には新しい版だけが出る。
	page := decode[galleryResponse](t, e.do("GET", "/api/scans", nil))
	if len(page.Items) != 1 || page.Items[0].ID != created.ID || page.Items[0].ParentID != parentID || page.Items[0].BaseFrameCount != 3 {
		t.Fatalf("一覧 = %+v", page.Items)
	}

	// 続きの版のさらに続きも撮れる（番号は 6 から）。
	body, ct = resumeBody(t, 1)
	rec = e.do("POST", "/api/scans/"+created.ID+"/resume", body, ct)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("2回目の resume = %d %s", rec.Code, rec.Body.String())
	}
	third, _ := e.root.Scan(decode[createResponse](t, rec).ID)
	if _, err := os.Stat(filepath.Join(third.ImagesDir(), "frame_0006.jpg")); err != nil {
		t.Fatalf("続きの番号で保存されていない: %v", err)
	}
}

func TestResumeRequiresMap(t *testing.T) {
	e := newTestEnv(t, nil)
	id := e.createScan(3) // ワーカーを動かさないので queued のまま（地図が無い）
	body, ct := resumeBody(t, 1)
	wantError(t, e.do("POST", "/api/scans/"+id+"/resume", body, ct), http.StatusConflict, "not_resumable")
	body, ct = resumeBody(t, 1)
	wantError(t, e.do("POST", "/api/scans/"+id+"/localize", body, ct), http.StatusConflict, "not_resumable")
	body, ct = resumeBody(t, 1)
	wantError(t, e.do("POST", "/api/scans/00000000-0000-4000-8000-000000000000/resume", body, ct), http.StatusNotFound, "not_found")
	if got := decode[scanResponse](t, e.do("GET", "/api/scans/"+id, nil)); got.Resumable {
		t.Fatal("処理前のスキャンが resumable になっている")
	}
	// 別のサイトからは受け付けない。
	body, ct = resumeBody(t, 1)
	wantError(t, e.do("POST", "/api/scans/"+id+"/resume", body, ct, header("Origin", "https://evil.example")), http.StatusForbidden, "cross_origin")
}

// localizeBody は写真 n 枚を項目 "image" に入れたボディ。
func localizeBody(t *testing.T, n int) (io.Reader, func(*http.Request)) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i < n; i++ {
		fw, _ := w.CreateFormFile("image", "query.jpg")
		fw.Write(photo(t, uint8(200+i)))
	}
	w.Close()
	return &body, header("Content-Type", w.FormDataContentType())
}

func TestLocalize(t *testing.T) {
	e := newTestEnv(t, nil)
	e.startWorker()
	id := e.createScan(3)
	e.waitScan(id)
	loc := e.server.localizer.(*fakeLocalizer)

	loc.result = vps.Result{Localized: true, Position: [3]float64{1, 2, 3}, Forward: [3]float64{0, 0, 1}, Matches: 321, ComparedImages: 3, MapImages: 3}
	body, ct := localizeBody(t, 1)
	rec := e.do("POST", "/api/scans/"+id+"/localize", body, ct)
	if rec.Code != 200 {
		t.Fatalf("localize = %d %s", rec.Code, rec.Body.String())
	}
	got := decode[localizeResponse](t, rec)
	if !got.Localized || got.Position == nil || *got.Position != [3]float64{1, 2, 3} || got.Matches != 321 || got.Message == "" {
		t.Fatalf("結果 = %+v", got)
	}
	if !loc.fileExist || loc.got.Width != 64 || loc.got.Height != 48 {
		t.Fatalf("位置合わせに渡した写真 = %+v（存在: %v）", loc.got, loc.fileExist)
	}
	// 受け取った写真は一時ディレクトリごと消える（地図にも作業ディレクトリにも残らない）。
	if _, err := os.Stat(loc.got.Path); !os.IsNotExist(err) {
		t.Fatalf("写真が残っている: %v", err)
	}

	loc.result = vps.Result{Localized: false, ComparedImages: 3, MapImages: 3}
	body, ct = localizeBody(t, 1)
	if got := decode[localizeResponse](t, e.do("POST", "/api/scans/"+id+"/localize", body, ct)); got.Localized || got.Position != nil || got.Message == "" {
		t.Fatalf("見つからないときの結果 = %+v", got)
	}

	body, ct = localizeBody(t, 2)
	wantError(t, e.do("POST", "/api/scans/"+id+"/localize", body, ct), http.StatusRequestEntityTooLarge, "too_many_images")
	body, ct = localizeBody(t, 0)
	wantError(t, e.do("POST", "/api/scans/"+id+"/localize", body, ct), http.StatusBadRequest, "too_few_images")

	loc.err = vps.ErrBusy
	body, ct = localizeBody(t, 1)
	wantError(t, e.do("POST", "/api/scans/"+id+"/localize", body, ct), http.StatusTooManyRequests, "busy")
	loc.err = vps.ErrTimeout
	body, ct = localizeBody(t, 1)
	wantError(t, e.do("POST", "/api/scans/"+id+"/localize", body, ct), http.StatusGatewayTimeout, "localize_timeout")
	loc.err = errors.New("colmap が終了コード 1 で失敗しました")
	body, ct = localizeBody(t, 1)
	wantError(t, e.do("POST", "/api/scans/"+id+"/localize", body, ct), http.StatusInternalServerError, "localize_failed")

	wantError(t, e.do("POST", "/api/scans/"+id+"/localize", strings.NewReader("{}"), header("Content-Type", "application/json")),
		http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func TestDeferredSplatAndRegenerate(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config) {
		c.Pipeline.EnableSplat = true
		c.Jobs.DeferSplat = true
	})
	e.startWorker() // 3DGS のワーカーは動かさない（待ち行列に残ったままを確かめる）
	id := e.createScan(3)
	got := e.waitScan(id)
	// 完成の記録の直後に待ち行列に入るので、少し待つ。
	for deadline := time.Now().Add(5 * time.Second); got.SplatJob == nil && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		got = decode[scanResponse](t, e.do("GET", "/api/scans/"+id, nil))
	}
	if got.Status != scan.StatusSucceeded {
		t.Fatalf("status = %s（メッシュが出来た時点で完成のはず）", got.Status)
	}
	if got.SplatJob == nil || got.SplatJob.Status != scan.SplatQueued || got.SplatJob.QueuePosition == nil || *got.SplatJob.QueuePosition != 0 {
		t.Fatalf("splatJob = %+v（順番待ちのはず）", got.SplatJob)
	}
	page := decode[galleryResponse](t, e.do("GET", "/api/scans", nil))
	if len(page.Items) != 1 || !page.Items[0].SplatPending {
		t.Fatalf("一覧 = %+v", page.Items)
	}
	// 既に待ち行列にあるので、作り直しは受け付けない。
	wantError(t, e.do("POST", "/api/scans/"+id+"/splat", nil), http.StatusConflict, "splat_busy")

	// 3DGS のワーカーを動かすと、待ち行列から学習して片付く。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.jobs.RunSplats(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	e.jobs.NotifySplat()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := decode[scanResponse](t, e.do("GET", "/api/scans/"+id, nil))
		if s.SplatJob == nil && len(s.Assets) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("3DGS が出来ない: %+v", s)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 作り直し（GPU を使えるようになった後など）。
	rec := e.do("POST", "/api/scans/"+id+"/splat", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("作り直し = %d %s", rec.Code, rec.Body.String())
	}

	// 成果物は作り直すと中身が変わるので、使うたびに確かめさせる（no-cache）。
	asset := e.do("GET", "/api/scans/"+id+"/assets/mesh_glb", nil)
	if cc := asset.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Fatalf("Cache-Control = %q", cc)
	}
}

func TestMakeSplatRejections(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config) { c.Pipeline.EnableSplat = true })
	id := e.createScan(3) // 処理前（地図が無い）
	wantError(t, e.do("POST", "/api/scans/"+id+"/splat", nil), http.StatusConflict, "not_ready")
	wantError(t, e.do("POST", "/api/scans/00000000-0000-4000-8000-000000000000/splat", nil), http.StatusNotFound, "not_found")
	wantError(t, e.do("POST", "/api/scans/"+id+"/splat", nil, header("Origin", "https://evil.example")), http.StatusForbidden, "cross_origin")

	off := newTestEnv(t, nil) // ENABLE_SPLAT=false
	off.startWorker()
	id = off.createScan(3)
	off.waitScan(id)
	wantError(t, off.do("POST", "/api/scans/"+id+"/splat", nil), http.StatusConflict, "splat_disabled")
}

// TestFocalHintIsSaved: 撮影側が送った焦点距離（EXIF から読んだ値）を、位置推定の手掛かりとして保存する。
func TestFocalHintIsSaved(t *testing.T) {
	e := newTestEnv(t, nil)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i < 3; i++ {
		fw, _ := w.CreateFormFile("images", "IMG.jpg")
		fw.Write(photo(t, uint8(i+1)))
	}
	w.WriteField("metadata", `{"frames":[{"focalLength35mm":26},{"focalLength35mm":26},{"focalLength35mm":26}]}`)
	w.Close()
	rec := e.do("POST", "/api/scans", &body, header("Content-Type", w.FormDataContentType()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("アップロード = %d %s", rec.Code, rec.Body.String())
	}
	ws, _ := e.root.Scan(decode[createResponse](t, rec).ID)
	hint, ok := reconstruction.ReadCameraHint(ws)
	if !ok || hint != (reconstruction.CameraHint{FocalLength35mm: 26, Width: 64, Height: 48}) {
		t.Fatalf("手掛かり = %+v, %v", hint, ok)
	}
	// 送られていなければ作らない。
	id := e.createScan(3)
	ws, _ = e.root.Scan(id)
	if _, ok := reconstruction.ReadCameraHint(ws); ok {
		t.Fatal("焦点距離が無いのに手掛かりを作った")
	}
}
