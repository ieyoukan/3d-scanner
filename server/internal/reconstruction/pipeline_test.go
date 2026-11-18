package reconstruction

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/cmdrun"
	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// このテストは COLMAP 等を実行しない。偽の Runner が「呼ばれたコマンドを記録し、
// そのコマンドが作るはずのファイルを置く」ことで、パイプラインの
//   - 実行順と引数
//   - 途中で失敗したときの振る舞い（部分成功）
//   - 出力ファイルの検証
// を確かめる。偽物が通っても「3D復元が正しい」ことの証明にはならない点に注意
// （本物の実行結果は docs/research.md と README の検証記録を参照）。

// fakeRunner は cmdrun.Runner の偽物。
type fakeRunner struct {
	mu sync.Mutex
	// calls は呼ばれた順の「ツール名 サブコマンド」（例: "colmap mapper", "opensplat"）。
	calls []string
	// args は呼ばれたコマンドの引数全体（キーは calls と同じ）。
	args map[string][]string
	// fail は「このコマンドが呼ばれたらこのエラーを返す」。
	fail map[string]error
	// noOutput は「成功を返すが出力ファイルを作らない」コマンド。
	noOutput map[string]bool
	// garbage は「成功を返すが中身がでたらめなファイルを作る」コマンド。
	garbage map[string]bool
	// registered は SfM で位置が分かったことにする画像の枚数。
	registered int
	// onCall は各コマンドの実行前に呼ばれる（キャンセルのテスト用）。
	onCall func(key string)
	// colmapBanner は `colmap version` の出力。
	colmapBanner string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		args: map[string][]string{}, fail: map[string]error{}, noOutput: map[string]bool{}, garbage: map[string]bool{},
		registered:   5,
		colmapBanner: "COLMAP 4.2.1 (Commit 0123abc on 2026-09-29 without GPU support, flat hash maps)\n",
	}
}

func (f *fakeRunner) Run(ctx context.Context, cmd cmdrun.Command) (cmdrun.Result, error) {
	key := filepath.Base(cmd.Name)
	if key == "colmap" && len(cmd.Args) > 0 {
		key += " " + cmd.Args[0]
		// model_converter は出力形式で2回使い分けるので区別する。
		if cmd.Args[0] == "model_converter" {
			key += " " + argValue(cmd.Args, "--output_type")
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.args[key] = cmd.Args
	f.mu.Unlock()

	if f.onCall != nil {
		f.onCall(key)
	}
	if err := ctx.Err(); err != nil {
		return cmdrun.Result{}, fmt.Errorf("%s を中止しました: %w", key, err)
	}
	if err := f.fail[key]; err != nil {
		return cmdrun.Result{}, err
	}
	if key == "colmap version" {
		return cmdrun.Result{OutputTail: f.colmapBanner}, nil
	}
	if f.noOutput[key] {
		return cmdrun.Result{}, nil
	}
	return cmdrun.Result{}, f.produce(key, cmd.Args)
}

// produce は本物のコマンドが作るファイルを、最小限の中身で置く。
func (f *fakeRunner) produce(key string, args []string) error {
	write := func(path, content string) error {
		if f.garbage[key] {
			content = "これは期待された形式ではありません" + strings.Repeat("x", 2000)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(content), 0o640)
	}
	pad := strings.Repeat("\x00", 2000)

	switch key {
	case "colmap mapper":
		dir := filepath.Join(argValue(args, "--output_path"), "0")
		if argValue(args, "--input_path") != "" {
			// 既存のモデルから続けるときは、出力先に直接書く（本物の COLMAP と同じ）。
			dir = argValue(args, "--output_path")
		}
		for _, name := range []string{"cameras.bin", "images.bin", "points3D.bin"} {
			if err := write(filepath.Join(dir, name), "bin"); err != nil {
				return err
			}
		}
	case "colmap model_converter TXT":
		out := argValue(args, "--output_path")
		var images strings.Builder
		images.WriteString("# Image list with two lines of data per image:\n")
		for i := 1; i <= f.registered; i++ {
			// 単位四元数（回転なし）で、カメラを x 方向に並べる。2行目（特徴点）は空行。
			fmt.Fprintf(&images, "%d 1 0 0 0 %d 0 0 1 frame_%04d.jpg\n\n", i, -i, i)
		}
		if err := write(filepath.Join(out, "images.txt"), images.String()); err != nil {
			return err
		}
		points := "# 3D point list\n1 0 0 5 255 0 0 0.5 1 0 2 0\n2 1 0 5 0 255 0 0.7 1 1 2 1\n3 0 1 5 0 0 255 0.6 1 2 2 2\n"
		return write(filepath.Join(out, "points3D.txt"), points)
	case "colmap model_converter PLY":
		return write(argValue(args, "--output_path"), "ply\nformat binary_little_endian 1.0\n"+pad)
	case "colmap image_undistorter":
		return os.MkdirAll(filepath.Join(argValue(args, "--output_path"), "sparse"), 0o750)
	case "colmap stereo_fusion":
		return write(argValue(args, "--output_path"), "ply\nformat ascii 1.0\nelement vertex 9000\nproperty float x\nend_header\n"+pad)
	case "colmap delaunay_mesher", "colmap poisson_mesher":
		return write(argValue(args, "--output_path"), asciiMeshPLY(false)+pad)
	case "colmap mesh_texturer":
		out := argValue(args, "--output_path")
		if err := write(filepath.Join(out, "mesh.ply"), asciiMeshPLY(true)); err != nil {
			return err
		}
		return write(filepath.Join(out, "texture.png"), "\x89PNG\r\n\x1a\n"+pad)
	case "gltfpack":
		return write(argValue(args, "-o"), "glTF"+pad)
	case "opensplat":
		return write(argValue(args, "-o"), "ply\nformat binary_little_endian 1.0\nelement vertex 12345\nproperty float x\nend_header\n"+pad)
	case "splat-transform":
		return write(args[len(args)-1], "PK\x03\x04"+pad)
	}
	return nil
}

// asciiMeshPLY は三角形2枚の小さなメッシュ（ascii 形式）。
func asciiMeshPLY(textured bool) string {
	var b strings.Builder
	b.WriteString("ply\nformat ascii 1.0\n")
	if textured {
		b.WriteString("comment TextureFile texture.png\n")
	}
	b.WriteString("element vertex 4\nproperty float x\nproperty float y\nproperty float z\nelement face 2\n")
	if textured {
		b.WriteString("property list uchar int vertex_indices\nproperty list uchar float texcoord\nend_header\n")
		b.WriteString("0 0 0\n1 0 0\n1 1 0\n0 1 0\n")
		b.WriteString("3 0 1 2 6 0 0 1 0 1 1\n3 0 2 3 6 0 0 1 1 0 1\n")
	} else {
		b.WriteString("property list uchar int vertex_index\nend_header\n")
		b.WriteString("0 0 0\n1 0 0\n1 1 0\n0 1 0\n3 0 1 2\n3 0 2 3\n")
	}
	return b.String()
}

// argValue は args から「flag の次の値」を取り出す。
func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// fakeReporter は通知された内容を記録する。
type fakeReporter struct {
	mu       sync.Mutex
	started  []scan.StepName
	finished map[scan.StepName]scan.StepStatus
	messages map[scan.StepName]string
	assets   map[scan.AssetKind]int64
}

func newFakeReporter() *fakeReporter {
	return &fakeReporter{finished: map[scan.StepName]scan.StepStatus{}, messages: map[scan.StepName]string{}, assets: map[scan.AssetKind]int64{}}
}

func (r *fakeReporter) StepStarted(_ context.Context, step scan.StepName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, step)
}

func (r *fakeReporter) StepFinished(_ context.Context, step scan.StepName, status scan.StepStatus, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished[step] = status
	r.messages[step] = message
}

func (r *fakeReporter) AssetReady(_ context.Context, kind scan.AssetKind, _ string, size int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assets[kind] = size
}

// outcome は記録からスキャン全体の状態を求める（ワーカーと同じ scan.Summarize を使う）。
func (r *fakeReporter) outcome() scan.Outcome {
	var steps []scan.Step
	for _, name := range scan.StepOrder {
		steps = append(steps, scan.Step{Name: name, Status: r.finished[name], ErrorMessage: r.messages[name]})
	}
	var kinds []scan.AssetKind
	for k := range r.assets {
		kinds = append(kinds, k)
	}
	return scan.Summarize(steps, kinds)
}

func testPipelineConfig() config.Pipeline {
	return config.Pipeline{
		ColmapBin: "colmap", OpenSplatBin: "opensplat", GltfpackBin: "gltfpack", SplatTransformBin: "splat-transform",
		EnableMesh: true, EnableSplat: true, MeshMode: config.MeshModeAuto,
		ColmapMaxImageSize: 1600, SplatIterations: 300, SplatDownscale: 2, SplatForceCPU: true,
		SfMTimeout: time.Minute, MVSTimeout: time.Minute, MeshTimeout: time.Minute, SplatTimeout: time.Minute, SOGTimeout: time.Minute,
	}
}

// newJob は写真5枚（同じ寸法）の入った作業ディレクトリを作る。
func newJob(t *testing.T) Job {
	t.Helper()
	root, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := scan.NewID()
	ws, err := root.Scan(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Create(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		writeJPEG(t, filepath.Join(ws.ImagesDir(), fmt.Sprintf("frame_%04d.jpg", i)), 64, 48)
	}
	return Job{ID: id, WS: ws}
}

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
}

func wantSteps(t *testing.T, rep *fakeReporter, want map[scan.StepName]scan.StepStatus) {
	t.Helper()
	for _, name := range scan.StepOrder {
		if got := rep.finished[name]; got != want[name] {
			t.Errorf("ステップ %s = %q, want %q（説明: %s）", name, got, want[name], rep.messages[name])
		}
	}
}

func wantAssets(t *testing.T, rep *fakeReporter, want ...scan.AssetKind) {
	t.Helper()
	if len(rep.assets) != len(want) {
		t.Errorf("成果物 = %v, want %v", rep.assets, want)
	}
	for _, k := range want {
		if rep.assets[k] <= 0 {
			t.Errorf("成果物 %s が登録されていない", k)
		}
	}
}

// TestRunSparseModeFullSuccess: GPU の無い環境での正常系。コマンドの順番そのものが仕様。
func TestRunSparseModeFullSuccess(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	p := New(testPipelineConfig(), run, Capabilities{ColmapGPU: false}, nil)

	if err := p.Run(context.Background(), job, rep); err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantCalls := []string{
		// SfM
		"colmap feature_extractor", "colmap exhaustive_matcher", "colmap mapper",
		"colmap model_converter TXT", "colmap model_converter PLY",
		// メッシュ分岐（疎）: 密な復元（patch_match_stereo / stereo_fusion）は呼ばれない
		"colmap image_undistorter", "colmap delaunay_mesher", "colmap mesh_texturer", "gltfpack",
		// 3DGS 分岐
		"opensplat", "splat-transform",
	}
	if strings.Join(run.calls, " | ") != strings.Join(wantCalls, " | ") {
		t.Fatalf("実行順が違う:\n got: %v\nwant: %v", run.calls, wantCalls)
	}

	wantSteps(t, rep, map[scan.StepName]scan.StepStatus{
		scan.StepSfM: scan.StepSucceeded, scan.StepMVS: scan.StepSkipped, scan.StepMesh: scan.StepSucceeded,
		scan.StepSplat: scan.StepSucceeded, scan.StepSOG: scan.StepSucceeded,
	})
	wantAssets(t, rep, scan.AssetSparsePLY, scan.AssetSceneJSON, scan.AssetMeshGLB, scan.AssetGaussianPLY, scan.AssetGaussianSOG)
	if got := rep.outcome().Status; got != scan.StatusSucceeded {
		t.Errorf("全体 = %q, want succeeded", got)
	}

	// スキップの理由が利用者に分かる形で残っている。
	if msg := rep.messages[scan.StepMVS]; !strings.Contains(msg, "CUDA") {
		t.Errorf("MVS を省いた理由が説明されていない: %q", msg)
	}
	// 成功メッセージに実際の数値が入っている（作り物の進捗ではない）。
	if msg := rep.messages[scan.StepSfM]; !strings.Contains(msg, "5枚中5枚") || !strings.Contains(msg, "3個の3D点") {
		t.Errorf("SfM の要約 = %q", msg)
	}
	if msg := rep.messages[scan.StepSplat]; !strings.Contains(msg, "12345個") {
		t.Errorf("3DGS の要約 = %q", msg)
	}

	// テクスチャ付き PLY が OBJ+MTL に変換され、gltfpack に渡されている。
	objPath := argValue(run.args["gltfpack"], "-i")
	if obj, err := os.ReadFile(objPath); err != nil || !strings.Contains(string(obj), "mtllib mesh.mtl") || !strings.Contains(string(obj), "f 1/1 2/2 3/3") {
		t.Errorf("OBJ が期待どおりでない（err=%v）:\n%s", err, obj)
	}
	if mtl, err := os.ReadFile(strings.TrimSuffix(objPath, ".obj") + ".mtl"); err != nil || !strings.Contains(string(mtl), "map_Kd texture.png") {
		t.Errorf("MTL がテクスチャを参照していない（err=%v）: %s", err, mtl)
	}
}

// TestRunCommandArguments: 外部CLIに渡す引数が、調査した仕様どおりであることを固定する。
func TestRunCommandArguments(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	cfg := testPipelineConfig()
	cfg.NumThreads = 3
	p := New(cfg, run, Capabilities{}, nil)
	if err := p.Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	ws := job.WS

	check := func(key string, want ...string) {
		t.Helper()
		got := strings.Join(run.args[key], " ")
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s の引数に %q が無い:\n  %s", key, w, got)
			}
		}
	}
	check("colmap feature_extractor",
		"--database_path "+ws.DatabasePath(), "--image_path "+ws.ImagesDir(),
		"--ImageReader.camera_model SIMPLE_RADIAL",
		"--ImageReader.single_camera 1", // 全て同じ寸法 → カメラを共有
		"--FeatureExtraction.use_gpu 0", "--FeatureExtraction.max_image_size 1600", "--FeatureExtraction.num_threads 3")
	check("colmap exhaustive_matcher", "--database_path "+ws.DatabasePath(), "--FeatureMatching.use_gpu 0", "--FeatureMatching.num_threads 3")
	check("colmap mapper", "--output_path "+ws.SparseDir(), "--Mapper.num_threads 3")
	check("colmap model_converter PLY", "--input_path "+ws.ModelDir(), "--output_type PLY")
	check("colmap image_undistorter", "--input_path "+ws.ModelDir(), "--output_path "+ws.DenseDir(), "--output_type COLMAP", "--max_image_size 1600")
	check("colmap delaunay_mesher", "--input_path "+filepath.Join(ws.DenseDir(), "sparse"), "--input_type sparse")
	check("colmap mesh_texturer", "--workspace_path "+ws.DenseDir(), "--output_path "+filepath.Join(ws.DenseDir(), "textured"))
	check("gltfpack", "-o "+filepath.Join(ws.OutputDir(), "mesh.glb"), "-noq")
	// OpenSplat にはプロジェクトのルート（images/ と sparse/0/ の親）を渡す。
	if got := run.args["opensplat"]; got[0] != ws.Dir() {
		t.Errorf("opensplat の第1引数 = %q, want %q", got[0], ws.Dir())
	}
	// 学習と圧縮は別の名前に書いてから置き換える（作り直しの途中で前の 3DGS を壊さない）。
	check("opensplat", "-n 300", "-d 2", "-o "+filepath.Join(ws.OutputDir(), "splat.new.ply"), "--cpu")
	check("splat-transform", "-w -g cpu "+filepath.Join(ws.OutputDir(), "splat.ply")+" "+filepath.Join(ws.OutputDir(), "scene.new.sog"))
	for _, name := range []string{"splat.ply", "scene.sog"} {
		if _, err := os.Stat(filepath.Join(ws.OutputDir(), name)); err != nil {
			t.Errorf("%s が置き換わっていない: %v", name, err)
		}
	}
	for _, name := range []string{"splat.new.ply", "scene.new.sog"} {
		if _, err := os.Stat(filepath.Join(ws.OutputDir(), name)); !os.IsNotExist(err) {
			t.Errorf("%s が残っている", name)
		}
	}
}

// TestRunMixedImageSizesDisablesSingleCamera: 寸法の違う写真が混ざっていたらカメラを共有しない。
func TestRunMixedImageSizesDisablesSingleCamera(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	writeJPEG(t, filepath.Join(job.WS.ImagesDir(), "frame_0006.jpg"), 80, 48)
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	if err := p.Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	if got := argValue(run.args["colmap feature_extractor"], "--ImageReader.single_camera"); got != "0" {
		t.Errorf("single_camera = %q, want 0", got)
	}
}

// TestRunDenseModeWithGPU: GPU 対応の COLMAP なら密な復元を通る（コマンドの順番と引数のみ確認。実機は未検証）。
func TestRunDenseModeWithGPU(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	p := New(testPipelineConfig(), run, Capabilities{ColmapGPU: true}, nil)
	if err := p.Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(run.calls, " | ")
	want := "colmap image_undistorter | colmap patch_match_stereo | colmap stereo_fusion | colmap poisson_mesher | colmap mesh_texturer | gltfpack"
	if !strings.Contains(got, want) {
		t.Fatalf("密な復元の順番が違う:\n got: %s\nwant(部分): %s", got, want)
	}
	if strings.Count(got, "colmap image_undistorter") != 1 {
		t.Errorf("image_undistorter は1回だけ実行するはず: %s", got)
	}
	wantSteps(t, rep, map[scan.StepName]scan.StepStatus{
		scan.StepSfM: scan.StepSucceeded, scan.StepMVS: scan.StepSucceeded, scan.StepMesh: scan.StepSucceeded,
		scan.StepSplat: scan.StepSucceeded, scan.StepSOG: scan.StepSucceeded,
	})
	if !strings.Contains(strings.Join(run.args["colmap patch_match_stereo"], " "), "--PatchMatchStereo.geom_consistency true") {
		t.Errorf("patch_match_stereo の引数 = %v", run.args["colmap patch_match_stereo"])
	}
	if !strings.Contains(rep.messages[scan.StepMVS], "9000点") {
		t.Errorf("MVS の要約 = %q", rep.messages[scan.StepMVS])
	}
}

func TestMeshModeOverride(t *testing.T) {
	cfg := testPipelineConfig()
	cfg.MeshMode = config.MeshModeSparse
	if New(cfg, newFakeRunner(), Capabilities{ColmapGPU: true}, nil).DenseMesh() {
		t.Error("MESH_MODE=sparse なのに dense になった")
	}
	cfg.MeshMode = config.MeshModeDense
	if !New(cfg, newFakeRunner(), Capabilities{ColmapGPU: false}, nil).DenseMesh() {
		t.Error("MESH_MODE=dense なのに sparse になった")
	}
}

// TestRunSfMFailureSkipsEverything: SfM が失敗したら後段は一切実行せず、理由つきでスキップにする。
func TestRunSfMFailureSkipsEverything(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	run.fail["colmap mapper"] = &cmdrun.ExitError{Command: "colmap", ExitCode: 1, StderrTail: "E mapper failed"}
	p := New(testPipelineConfig(), run, Capabilities{}, nil)

	if err := p.Run(context.Background(), job, rep); err != nil {
		t.Fatalf("ステップの失敗は Run のエラーにしない: %v", err)
	}
	if last := run.calls[len(run.calls)-1]; last != "colmap mapper" {
		t.Errorf("失敗後もコマンドを実行した: %v", run.calls)
	}
	wantSteps(t, rep, map[scan.StepName]scan.StepStatus{
		scan.StepSfM: scan.StepFailed, scan.StepMVS: scan.StepSkipped, scan.StepMesh: scan.StepSkipped,
		scan.StepSplat: scan.StepSkipped, scan.StepSOG: scan.StepSkipped,
	})
	wantAssets(t, rep)
	out := rep.outcome()
	if out.Status != scan.StatusFailed || out.ErrorCode != "sfm_failed" {
		t.Errorf("全体 = %+v", out)
	}
	if msg := rep.messages[scan.StepSfM]; !strings.Contains(msg, "終了コード 1") {
		t.Errorf("失敗の説明 = %q", msg)
	}
}

// TestRunSfMWithoutModel: mapper は 0 で終わったがモデルを作らなかった（写真がつながらない）場合。
// 「コマンドが成功した」ことと「結果がある」ことは別、の代表例。
func TestRunSfMWithoutModel(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	run.noOutput["colmap mapper"] = true
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	p.Run(context.Background(), job, rep)

	if rep.finished[scan.StepSfM] != scan.StepFailed {
		t.Fatalf("モデルが無いのに SfM が %q", rep.finished[scan.StepSfM])
	}
	if msg := rep.messages[scan.StepSfM]; !strings.Contains(msg, "重なる") {
		t.Errorf("撮り直しの助言が無い: %q", msg)
	}
}

// TestRunSfMMapperReportsNoModel: COLMAP 4.2.1 の mapper は、写真がつながらないとき
// 「Failed to create any sparse model」と出して終了コード 1 で終わる（実機で確認した挙動）。
// 利用者には「終了コード 1」ではなく、撮り直し方の分かる説明を出す。
func TestRunSfMMapperReportsNoModel(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	run.fail["colmap mapper"] = &cmdrun.ExitError{
		Command: "colmap", ExitCode: 1,
		StderrTail: "W incremental_pipeline.cc:390] No images with matches\nE sfm.cc:310] Failed to create any sparse model\n",
	}
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	p.Run(context.Background(), job, rep)

	msg := rep.messages[scan.StepSfM]
	if rep.finished[scan.StepSfM] != scan.StepFailed || !strings.Contains(msg, "重なる") || strings.Contains(msg, "終了コード") {
		t.Fatalf("SfM = %q / %q", rep.finished[scan.StepSfM], msg)
	}
	// それ以外の理由での失敗は、そのまま伝える（原因を取り違えない）。
	run2, rep2, job2 := newFakeRunner(), newFakeReporter(), newJob(t)
	run2.fail["colmap mapper"] = &cmdrun.ExitError{Command: "colmap", ExitCode: 1, StderrTail: "database is locked"}
	New(testPipelineConfig(), run2, Capabilities{}, nil).Run(context.Background(), job2, rep2)
	if msg := rep2.messages[scan.StepSfM]; !strings.Contains(msg, "終了コード 1") {
		t.Fatalf("別の原因の失敗 = %q", msg)
	}
}

func TestRunSfMTooFewRegisteredImages(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	run.registered = 2
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	p.Run(context.Background(), job, rep)
	if rep.finished[scan.StepSfM] != scan.StepFailed || !strings.Contains(rep.messages[scan.StepSfM], "5枚中2枚") {
		t.Fatalf("SfM = %q / %q", rep.finished[scan.StepSfM], rep.messages[scan.StepSfM])
	}
}

// TestRunPartialSuccess: 分岐は独立。片方の失敗がもう片方を止めない。
func TestRunPartialSuccess(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(r *fakeRunner)
		wantSteps  map[scan.StepName]scan.StepStatus
		wantAssets []scan.AssetKind
		wantStatus scan.Status
		wantCode   string
	}{
		{
			name: "Mesh が失敗しても 3DGS は成功する",
			setup: func(r *fakeRunner) {
				r.fail["colmap mesh_texturer"] = &cmdrun.ExitError{Command: "colmap", ExitCode: 1}
			},
			wantSteps: map[scan.StepName]scan.StepStatus{
				scan.StepSfM: scan.StepSucceeded, scan.StepMVS: scan.StepSkipped, scan.StepMesh: scan.StepFailed,
				scan.StepSplat: scan.StepSucceeded, scan.StepSOG: scan.StepSucceeded,
			},
			wantAssets: []scan.AssetKind{scan.AssetSparsePLY, scan.AssetSceneJSON, scan.AssetGaussianPLY, scan.AssetGaussianSOG},
			wantStatus: scan.StatusPartial, wantCode: "mesh_failed",
		},
		{
			name:  "3DGS が失敗しても Mesh は残る（SOG は実行しない）",
			setup: func(r *fakeRunner) { r.fail["opensplat"] = fmt.Errorf("opensplat: %w", cmdrun.ErrTimeout) },
			wantSteps: map[scan.StepName]scan.StepStatus{
				scan.StepSfM: scan.StepSucceeded, scan.StepMVS: scan.StepSkipped, scan.StepMesh: scan.StepSucceeded,
				scan.StepSplat: scan.StepFailed, scan.StepSOG: scan.StepSkipped,
			},
			wantAssets: []scan.AssetKind{scan.AssetSparsePLY, scan.AssetSceneJSON, scan.AssetMeshGLB},
			wantStatus: scan.StatusPartial, wantCode: "splat_failed",
		},
		{
			name: "SOG 変換だけ失敗しても PLY で閲覧できる",
			setup: func(r *fakeRunner) {
				r.fail["splat-transform"] = &cmdrun.ExitError{Command: "splat-transform", ExitCode: 2}
			},
			wantSteps: map[scan.StepName]scan.StepStatus{
				scan.StepSfM: scan.StepSucceeded, scan.StepMVS: scan.StepSkipped, scan.StepMesh: scan.StepSucceeded,
				scan.StepSplat: scan.StepSucceeded, scan.StepSOG: scan.StepFailed,
			},
			wantAssets: []scan.AssetKind{scan.AssetSparsePLY, scan.AssetSceneJSON, scan.AssetMeshGLB, scan.AssetGaussianPLY},
			wantStatus: scan.StatusPartial, wantCode: "sog_failed",
		},
		{
			name: "両方の分岐が失敗したら全体は失敗（疎な点群だけでは完成と呼ばない）",
			setup: func(r *fakeRunner) {
				r.fail["colmap delaunay_mesher"] = &cmdrun.ExitError{Command: "colmap", ExitCode: 1}
				r.fail["opensplat"] = &cmdrun.ExitError{Command: "opensplat", ExitCode: 137}
			},
			wantSteps: map[scan.StepName]scan.StepStatus{
				scan.StepSfM: scan.StepSucceeded, scan.StepMVS: scan.StepSkipped, scan.StepMesh: scan.StepFailed,
				scan.StepSplat: scan.StepFailed, scan.StepSOG: scan.StepSkipped,
			},
			wantAssets: []scan.AssetKind{scan.AssetSparsePLY, scan.AssetSceneJSON},
			wantStatus: scan.StatusFailed, wantCode: "mesh_failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
			c.setup(run)
			p := New(testPipelineConfig(), run, Capabilities{}, nil)
			if err := p.Run(context.Background(), job, rep); err != nil {
				t.Fatalf("Run: %v", err)
			}
			wantSteps(t, rep, c.wantSteps)
			wantAssets(t, rep, c.wantAssets...)
			out := rep.outcome()
			if out.Status != c.wantStatus || out.ErrorCode != c.wantCode {
				t.Errorf("全体 = %+v, want %s/%s", out, c.wantStatus, c.wantCode)
			}
		})
	}
}

// TestRunRejectsMissingOrBogusOutput: コマンドが 0 で終わっても、出力が無い・形式が違うなら失敗にする。
// 「API は成功したが中身はダミー」を防ぐ検証（指示書 §5）。
func TestRunRejectsMissingOrBogusOutput(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(r *fakeRunner)
		step     scan.StepName
		notAsset scan.AssetKind
	}{
		{"gltfpack が何も書かなかった", func(r *fakeRunner) { r.noOutput["gltfpack"] = true }, scan.StepMesh, scan.AssetMeshGLB},
		{"gltfpack の出力が GLB でない", func(r *fakeRunner) { r.garbage["gltfpack"] = true }, scan.StepMesh, scan.AssetMeshGLB},
		{"mesh_texturer がテクスチャを書かなかった", func(r *fakeRunner) { r.noOutput["colmap mesh_texturer"] = true }, scan.StepMesh, scan.AssetMeshGLB},
		{"opensplat が何も書かなかった", func(r *fakeRunner) { r.noOutput["opensplat"] = true }, scan.StepSplat, scan.AssetGaussianPLY},
		{"opensplat の出力が PLY でない", func(r *fakeRunner) { r.garbage["opensplat"] = true }, scan.StepSplat, scan.AssetGaussianPLY},
		{"splat-transform の出力が SOG でない", func(r *fakeRunner) { r.garbage["splat-transform"] = true }, scan.StepSOG, scan.AssetGaussianSOG},
		{"疎な点群の PLY が出力されなかった", func(r *fakeRunner) { r.noOutput["colmap model_converter PLY"] = true }, scan.StepSfM, scan.AssetSparsePLY},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
			c.setup(run)
			p := New(testPipelineConfig(), run, Capabilities{}, nil)
			p.Run(context.Background(), job, rep)
			if got := rep.finished[c.step]; got != scan.StepFailed {
				t.Errorf("ステップ %s = %q, want failed", c.step, got)
			}
			if _, ok := rep.assets[c.notAsset]; ok {
				t.Errorf("検証を通っていない成果物 %s が登録された", c.notAsset)
			}
		})
	}
}

// TestRunCancelled: 実行中に取り消されたら、それ以降は何も実行せず、失敗としても記録しない。
func TestRunCancelled(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	ctx, cancel := context.WithCancel(context.Background())
	run.onCall = func(key string) {
		if key == "colmap delaunay_mesher" {
			cancel() // メッシュ化の最中に利用者が中止した、という状況
		}
	}
	p := New(testPipelineConfig(), run, Capabilities{}, nil)

	err := p.Run(ctx, job, rep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if last := run.calls[len(run.calls)-1]; last != "colmap delaunay_mesher" {
		t.Errorf("取り消し後もコマンドを実行した: %v", run.calls)
	}
	// 取り消されたステップは failed にしない（状態はキャンセル処理が決める）。
	if st, ok := rep.finished[scan.StepMesh]; ok {
		t.Errorf("取り消されたステップが %q として報告された", st)
	}
	if _, ok := rep.finished[scan.StepSplat]; ok {
		t.Error("取り消し後に 3DGS 分岐が報告された")
	}
}

func TestRunDisabledBranches(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	cfg := testPipelineConfig()
	cfg.EnableSplat = false
	p := New(cfg, run, Capabilities{}, nil)
	if err := p.Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if c == "opensplat" || c == "splat-transform" {
			t.Errorf("無効にした分岐のコマンド %s が実行された", c)
		}
	}
	wantSteps(t, rep, map[scan.StepName]scan.StepStatus{
		scan.StepSfM: scan.StepSucceeded, scan.StepMVS: scan.StepSkipped, scan.StepMesh: scan.StepSucceeded,
		scan.StepSplat: scan.StepSkipped, scan.StepSOG: scan.StepSkipped,
	})
	if got := rep.outcome().Status; got != scan.StatusSucceeded {
		t.Errorf("全体 = %q, want succeeded（無効化は失敗ではない）", got)
	}
}

// TestSelectBestModelPrefersLargest: モデルが複数に分かれたら、登録画像の多い方を sparse/0 にする。
func TestSelectBestModelPrefersLargest(t *testing.T) {
	run, job := newFakeRunner(), newJob(t)
	ws := job.WS
	// sparse/0（小さい）と sparse/1（大きい）を用意し、中に目印のファイルを置く。
	for _, name := range []string{"0", "1"} {
		dir := filepath.Join(ws.SparseDir(), name)
		os.MkdirAll(dir, 0o750)
		os.WriteFile(filepath.Join(dir, "marker"), []byte(name), 0o640)
	}
	// 偽の model_converter が、モデル 0 には2枚、モデル 1 には4枚を登録済みとして書くようにする。
	run.onCall = func(key string) {
		if key == "colmap model_converter TXT" {
			run.mu.Lock()
			defer run.mu.Unlock()
			if strings.HasSuffix(argValue(run.args[key], "--input_path"), "1") {
				run.registered = 4
			} else {
				run.registered = 2
			}
		}
	}
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	info, err := p.selectBestModel(context.Background(), ws, ws.LogPath(scan.StepSfM), 5)
	if err != nil {
		t.Fatal(err)
	}
	if info.RegisteredImages != 4 {
		t.Errorf("選ばれたモデルの登録枚数 = %d, want 4", info.RegisteredImages)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.ModelDir(), "marker")); string(b) != "1" {
		t.Errorf("sparse/0 に大きい方のモデルが置かれていない（marker=%q）", b)
	}
}

func TestDetectCapabilities(t *testing.T) {
	cases := []struct {
		banner  string
		wantGPU bool
		wantVer string
	}{
		{"COLMAP 4.2.1 (Commit 0123abc on 2026-09-29 without GPU support, flat hash maps)\n", false, "4.2.1"},
		{"COLMAP 4.2.1 (Commit 0123abc on 2026-09-29 with CUDA, flat hash maps)\n", true, "4.2.1"},
		{"COLMAP 4.2.0 (Commit 0123abc on 2026-08-31 with HIP, flat hash maps)\n", true, "4.2.0"},
	}
	for _, c := range cases {
		run := newFakeRunner()
		run.colmapBanner = c.banner
		caps, err := DetectCapabilities(context.Background(), run, "colmap")
		if err != nil {
			t.Fatal(err)
		}
		if caps.ColmapGPU != c.wantGPU || caps.ColmapVersion != c.wantVer {
			t.Errorf("%q → %+v", c.banner, caps)
		}
	}

	run := newFakeRunner()
	run.fail["colmap version"] = errors.New("exec: not found")
	run.fail["colmap help"] = errors.New("exec: not found")
	if _, err := DetectCapabilities(context.Background(), run, "colmap"); err == nil {
		t.Error("colmap が無いのにエラーにならない")
	}
}

func TestUserMessage(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{friendly("写真が足りません"), "写真が足りません"},
		{fmt.Errorf("opensplat（制限 6h）: %w", cmdrun.ErrTimeout), "制限時間内"},
		{&cmdrun.ExitError{Command: "/usr/local/bin/opensplat", ExitCode: 137}, "メモリ不足"},
		{&cmdrun.ExitError{Command: "opensplat", ExitCode: -1, Signal: "SIGKILL"}, "メモリ不足"},
		// 実機で観測した例: 3D点が 53 個しか無い入力で colmap が SIGABRT で落ちた。メモリ不足とは言わない。
		{&cmdrun.ExitError{Command: "colmap", ExitCode: -1, Signal: "SIGABRT", StderrTail: "*** SIGABRT received"}, "少なすぎる"},
		{&cmdrun.ExitError{Command: "colmap", ExitCode: -1, Signal: "SIGSEGV"}, "SIGSEGV"},
		{&cmdrun.ExitError{Command: "colmap", ExitCode: 1, StderrTail: "Dense stereo reconstruction requires CUDA, which is not available"}, "CUDA"},
		{&cmdrun.ExitError{Command: "colmap", ExitCode: 1, StderrTail: "write failed: No space left on device"}, "ディスク"},
		{&cmdrun.ExitError{Command: "colmap", ExitCode: 3, StderrTail: "something else"}, "終了コード 3"},
	}
	for _, c := range cases {
		if got := userMessage(c.err); !strings.Contains(got, c.want) {
			t.Errorf("userMessage(%v) = %q, want %q を含む", c.err, got, c.want)
		}
	}
	// クラッシュ（SIGABRT）をメモリ不足と説明しない。
	if got := userMessage(&cmdrun.ExitError{Command: "colmap", ExitCode: -1, Signal: "SIGABRT"}); strings.Contains(got, "メモリ") {
		t.Errorf("SIGABRT をメモリ不足と説明している: %q", got)
	}
}

// newResumeJob は「続きを撮る」の作業ディレクトリを作る。写真5枚のうち、1〜3枚目が元の版から
// 引き継いだもの（base/frames.txt に記録）、4〜5枚目が今回の写真。
func newResumeJob(t *testing.T) Job {
	t.Helper()
	job := newJob(t)
	ws := job.WS
	if err := os.MkdirAll(ws.BaseModelDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		ws.BaseDatabasePath():                          "元の版の DB",
		filepath.Join(ws.BaseModelDir(), "images.bin"): "bin",
		ws.BaseFramesPath():                            "frame_0001.jpg\nframe_0002.jpg\nframe_0003.jpg\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return job
}

func TestResumeStartsFromInheritedMap(t *testing.T) {
	run := newFakeRunner()
	rep := newFakeReporter()
	job := newResumeJob(t)
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	if err := p.Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	if rep.finished[scan.StepSfM] != scan.StepSucceeded {
		t.Fatalf("SfM = %s: %s", rep.finished[scan.StepSfM], rep.messages[scan.StepSfM])
	}
	if msg := rep.messages[scan.StepSfM]; !strings.Contains(msg, "今回の2枚中2枚") {
		t.Fatalf("SfM の補足 = %q", msg)
	}

	// mapper は元の版のモデルから続け、元の写真のカメラ位置を固定し、sparse/0 に直接書く。
	args := run.args["colmap mapper"]
	if argValue(args, "--input_path") != job.WS.BaseModelDir() || argValue(args, "--output_path") != job.WS.ModelDir() ||
		argValue(args, "--Mapper.fix_existing_frames") != "1" {
		t.Fatalf("mapper の引数 = %v", args)
	}
	// 作業用の DB は元の版の複製（同じファイルを共有していない）。
	work, err1 := os.Stat(job.WS.DatabasePath())
	base, err2 := os.Stat(job.WS.BaseDatabasePath())
	if err1 != nil || err2 != nil || os.SameFile(work, base) {
		t.Fatalf("作業用の DB が複製になっていない: %v %v", err1, err2)
	}
	if b, _ := os.ReadFile(job.WS.DatabasePath()); string(b) != "元の版の DB" {
		t.Fatalf("作業用の DB の中身 = %q", b)
	}
	if out := rep.outcome(); out.Status != scan.StatusSucceeded {
		t.Fatalf("全体 = %+v", out)
	}
}

func TestResumeFailsWhenNoNewPhotoFits(t *testing.T) {
	run := newFakeRunner()
	run.registered = 3 // 元の3枚だけが地図に入り、今回の2枚は入らなかった
	rep := newFakeReporter()
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	if err := p.Run(context.Background(), newResumeJob(t), rep); err != nil {
		t.Fatal(err)
	}
	if rep.finished[scan.StepSfM] != scan.StepFailed || rep.messages[scan.StepSfM] != noNewImagesMessage {
		t.Fatalf("SfM = %s: %q", rep.finished[scan.StepSfM], rep.messages[scan.StepSfM])
	}
	if rep.finished[scan.StepSplat] != scan.StepSkipped {
		t.Fatalf("3DGS = %s（SfM の失敗で実行しないはず）", rep.finished[scan.StepSplat])
	}
	if out := rep.outcome(); out.Status != scan.StatusFailed {
		t.Fatalf("全体 = %+v", out)
	}
}

func TestNewScanDoesNotUseResumeOptions(t *testing.T) {
	run := newFakeRunner()
	p := New(testPipelineConfig(), run, Capabilities{}, nil)
	job := newJob(t)
	if err := p.Run(context.Background(), job, newFakeReporter()); err != nil {
		t.Fatal(err)
	}
	args := run.args["colmap mapper"]
	if argValue(args, "--input_path") != "" || argValue(args, "--output_path") != job.WS.SparseDir() {
		t.Fatalf("mapper の引数 = %v", args)
	}
}

// TestDeferSplatStopsAfterMesh: あとで作る設定では、メッシュが出来たら 3DGS を実行せずに戻る。
func TestDeferSplatStopsAfterMesh(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	job.DeferSplat = true
	if err := New(testPipelineConfig(), run, Capabilities{}, nil).Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	if _, called := run.args["opensplat"]; called {
		t.Fatal("あとで作るはずの 3DGS を実行した")
	}
	if _, done := rep.finished[scan.StepSplat]; done {
		t.Fatalf("splat のステップを記録した: %s", rep.finished[scan.StepSplat])
	}
	// メッシュがあるので、この時点で見られる（succeeded）。
	if out := rep.outcome(); out.Status != scan.StatusSucceeded {
		t.Fatalf("全体 = %+v", out)
	}

	// 後から RunSplat で 3DGS 分岐だけを実行する。
	if err := New(testPipelineConfig(), run, Capabilities{}, nil).RunSplat(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	if rep.finished[scan.StepSplat] != scan.StepSucceeded || rep.finished[scan.StepSOG] != scan.StepSucceeded {
		t.Fatalf("3DGS = %s / %s", rep.finished[scan.StepSplat], rep.finished[scan.StepSOG])
	}
	if _, ok := rep.assets[scan.AssetGaussianSOG]; !ok {
		t.Fatal("SOG が登録されていない")
	}
}

// TestDeferSplatRunsInlineWhenMeshFails: メッシュが出来なかったら、見られるものを作るため 3DGS もその場で作る。
func TestDeferSplatRunsInlineWhenMeshFails(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	job.DeferSplat = true
	run.fail["colmap delaunay_mesher"] = errors.New("失敗")
	if err := New(testPipelineConfig(), run, Capabilities{}, nil).Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	if rep.finished[scan.StepSplat] != scan.StepSucceeded {
		t.Fatalf("3DGS = %s（その場で作るはず）", rep.finished[scan.StepSplat])
	}
}

// TestFailedSplatKeepsPreviousFile: 作り直しに失敗しても、前の 3DGS のファイルは残る。
func TestFailedSplatKeepsPreviousFile(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t)
	prev := filepath.Join(job.WS.OutputDir(), "splat.ply")
	if err := os.WriteFile(prev, []byte("前の 3DGS"), 0o640); err != nil {
		t.Fatal(err)
	}
	run.garbage["opensplat"] = true
	if err := New(testPipelineConfig(), run, Capabilities{}, nil).RunSplat(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	if rep.finished[scan.StepSplat] != scan.StepFailed {
		t.Fatalf("3DGS = %s", rep.finished[scan.StepSplat])
	}
	if b, _ := os.ReadFile(prev); string(b) != "前の 3DGS" {
		t.Fatalf("前のファイルが書き換わった: %q", b)
	}
	if _, err := os.Stat(filepath.Join(job.WS.OutputDir(), "splat.new.ply")); !os.IsNotExist(err) {
		t.Fatal("作りかけのファイルが残っている")
	}
}

func TestCameraHintFocalPixels(t *testing.T) {
	// 26mm 相当・4000×3000 の写真: 対角 5000px ÷ 43.27mm × 26mm ≒ 3004px。
	h := CameraHint{FocalLength35mm: 26, Width: 4000, Height: 3000}
	if got := h.FocalPixels(); got < 3003 || got > 3006 {
		t.Fatalf("FocalPixels = %v", got)
	}
	for _, bad := range []CameraHint{{FocalLength35mm: 0, Width: 1, Height: 1}, {FocalLength35mm: 26}, {FocalLength35mm: 5000, Width: 1, Height: 1}} {
		if bad.Valid() {
			t.Errorf("%+v が有効とされた", bad)
		}
	}
}

// TestCameraHintPassedToColmap: 焦点距離の手掛かりがあり、写真がその寸法なら COLMAP に初期値として渡す。
func TestCameraHintPassedToColmap(t *testing.T) {
	run, rep, job := newFakeRunner(), newFakeReporter(), newJob(t) // 写真は 64×48
	if err := WriteCameraHint(job.WS, CameraHint{FocalLength35mm: 26, Width: 64, Height: 48}); err != nil {
		t.Fatal(err)
	}
	if err := New(testPipelineConfig(), run, Capabilities{}, nil).Run(context.Background(), job, rep); err != nil {
		t.Fatal(err)
	}
	// 対角 80px ÷ 43.27 × 26 ≒ 48.07px、中心は (32, 24)。
	if got := argValue(run.args["colmap feature_extractor"], "--ImageReader.camera_params"); got != "48.07,32.0,24.0,0" {
		t.Fatalf("camera_params = %q", got)
	}
	if msg := rep.messages[scan.StepSfM]; !strings.Contains(msg, "35mm 換算 26mm") {
		t.Fatalf("SfM の補足 = %q", msg)
	}

	// 寸法が合わなければ使わない。
	run, rep, job = newFakeRunner(), newFakeReporter(), newJob(t)
	WriteCameraHint(job.WS, CameraHint{FocalLength35mm: 26, Width: 4000, Height: 3000})
	New(testPipelineConfig(), run, Capabilities{}, nil).Run(context.Background(), job, rep)
	if got := argValue(run.args["colmap feature_extractor"], "--ImageReader.camera_params"); got != "" {
		t.Fatalf("寸法が違うのに渡した: %q", got)
	}
}
