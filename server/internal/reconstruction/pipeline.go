// Package reconstruction は写真から3Dデータを作る処理の「順番」と「成功の判定」を担当する。
//
// 全体の流れ（→ は外部CLIの呼び出し）:
//
//	写真 ─→ [SfM] カメラの位置・向き ＋ 疎な点群          （必須。ここが失敗したら全体が失敗）
//	          │
//	          ├─→ [MVS] 密な点群（CUDA があるときだけ）
//	          │     └─→ [Mesh] メッシュ化 → テクスチャ → GLB
//	          │
//	          └─→ [Splat] 3D Gaussian Splatting の学習 → PLY
//	                └─→ [SOG] Web 配信向けに圧縮
//
// 設計の要点:
//   - SfM の結果を2つの分岐が共有する。分岐どうしは独立で、片方が失敗してももう片方は実行する。
//   - 各ステップは「コマンドが 0 で終わった」だけでは成功にしない。
//     出力ファイルの存在・大きさ・先頭バイトを確かめてから成功を報告する。
//   - このパッケージは DB も HTTP も知らない。進行状況は Reporter に伝えるだけ。
//     だから、偽の Runner と Reporter を渡せば COLMAP 無しで順番と失敗時の振る舞いをテストできる。
package reconstruction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/cmdrun"
	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// Job は復元ジョブ1件の入力。
type Job struct {
	ID string
	// WS は作業ディレクトリ。images/ に写真が入っている。
	WS *workspace.Scan
}

// Reporter はパイプラインの進行状況を受け取る側（本番ではワーカーが DB に書く）。
type Reporter interface {
	// StepStarted はステップの開始を伝える。
	StepStarted(ctx context.Context, step scan.StepName)
	// StepFinished はステップの終了を伝える。
	// message は、成功・スキップなら補足、失敗なら利用者向けの理由（日本語）。
	StepFinished(ctx context.Context, step scan.StepName, status scan.StepStatus, message string)
	// AssetReady は検証済みの成果物が出来たことを伝える。
	AssetReady(ctx context.Context, kind scan.AssetKind, absPath string, sizeBytes int64)
}

// Capabilities は起動時に調べた外部CLIの能力。
type Capabilities struct {
	// ColmapVersion は "4.2.1" のような版番号（取れなければ空）。
	ColmapVersion string
	// ColmapGPU は COLMAP が CUDA（または HIP）つきでビルドされているか。
	// false だと patch_match_stereo（密な復元）が実行できない。
	ColmapGPU bool
}

var colmapVersionPattern = regexp.MustCompile(`COLMAP (\S+)`)

// DetectCapabilities は `colmap version` の出力から能力を調べる。
//
// 出力例: "COLMAP 4.2.1 (Commit 0123abc on 2026-09-29 without GPU support, ...)"
// CUDA つきなら "with CUDA"、AMD 向けなら "with HIP" と表示される（COLMAP の version.cc より）。
//
// 失敗条件: colmap を起動できない。能力が分からないまま動かすと、最初のジョブが数分走ってから
// 失敗するので、起動時に確かめて、だめなら理由つきで止める。
func DetectCapabilities(ctx context.Context, run cmdrun.Runner, colmapBin string) (Capabilities, error) {
	res, err := run.Run(ctx, cmdrun.Command{Name: colmapBin, Args: []string{"version"}})
	if err != nil || !strings.Contains(res.OutputTail, "COLMAP") {
		// 4.1 より前の COLMAP には version コマンドが無い。help の先頭行にも同じ情報が出る。
		res, err = run.Run(ctx, cmdrun.Command{Name: colmapBin, Args: []string{"help"}})
		if err != nil {
			return Capabilities{}, fmt.Errorf("colmap を実行できません: %w", err)
		}
	}
	caps := Capabilities{
		ColmapGPU: strings.Contains(res.OutputTail, "with CUDA") || strings.Contains(res.OutputTail, "with HIP"),
	}
	if m := colmapVersionPattern.FindStringSubmatch(res.OutputTail); m != nil {
		caps.ColmapVersion = m[1]
	}
	return caps, nil
}

// Pipeline は復元処理の実行器。
type Pipeline struct {
	cfg  config.Pipeline
	run  cmdrun.Runner
	caps Capabilities
	log  *slog.Logger
}

// New は Pipeline を作る。
func New(cfg config.Pipeline, run cmdrun.Runner, caps Capabilities, log *slog.Logger) *Pipeline {
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{cfg: cfg, run: run, caps: caps, log: log}
}

// DenseMesh は、メッシュ分岐で密な復元（MVS）を使うかどうかを返す。
//
//   - auto:   COLMAP が GPU 対応なら dense、そうでなければ sparse。
//   - dense:  常に dense（GPU が無ければ patch_match_stereo が失敗し、その失敗がそのまま画面に出る）。
//   - sparse: 常に sparse。
func (p *Pipeline) DenseMesh() bool {
	switch p.cfg.MeshMode {
	case config.MeshModeDense:
		return true
	case config.MeshModeSparse:
		return false
	}
	return p.caps.ColmapGPU
}

// Run は1件のジョブを最後まで実行する。
//
// 入力: job（写真の入った作業ディレクトリ）、rep（進行状況の通知先）。
// 出力: nil（各ステップの成否は rep 経由で伝える。「一部失敗」はエラーではない）。
// 副作用: 作業ディレクトリに中間ファイルと成果物を書き、外部プロセスを起動する。
// 失敗条件: ctx が取り消された（利用者のキャンセル、サーバーの停止）ときだけ、そのエラーを返す。
func (p *Pipeline) Run(ctx context.Context, job Job, rep Reporter) error {
	if err := p.step(ctx, rep, scan.StepSfM, func() (string, error) { return p.runSfM(ctx, job, rep) }); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// SfM が無いとカメラの位置が分からないので、どちらの分岐も始められない。
		for _, s := range []scan.StepName{scan.StepMVS, scan.StepMesh, scan.StepSplat, scan.StepSOG} {
			rep.StepFinished(ctx, s, scan.StepSkipped, "SfM が失敗したため実行していません。")
		}
		return nil
	}

	// 2つの分岐は並列ではなく順番に実行する。どちらも全コアとメモリを使うので、
	// 同時に走らせると互いに遅くなり、メモリ不足で落ちやすくなる。
	p.runMeshBranch(ctx, job, rep)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.runSplatBranch(ctx, job, rep)
	return ctx.Err()
}

// step は1ステップの「開始を通知 → 実行 → 結果を通知」を行う共通の枠。
// fn は成功時の補足メッセージを返す。
func (p *Pipeline) step(ctx context.Context, rep Reporter, name scan.StepName, fn func() (string, error)) error {
	rep.StepStarted(ctx, name)
	msg, err := fn()
	if err != nil {
		if ctx.Err() != nil {
			// 取り消しは「失敗」として記録しない（キャンセル側が状態を cancelled にする）。
			return err
		}
		p.log.Warn("ステップが失敗", "step", name, "error", err)
		rep.StepFinished(ctx, name, scan.StepFailed, userMessage(err))
		return err
	}
	rep.StepFinished(ctx, name, scan.StepSucceeded, msg)
	return nil
}

// ---------------------------------------------------------------------------
// SfM
// ---------------------------------------------------------------------------

// runSfM は Structure from Motion を実行する。
//
// 入力:  images/（写真）
// 出力:  database.db（特徴点とマッチ）、sparse/0/（カメラと疎な点群）、
//
//	output/sparse.ply（点群の確認用）、output/scene.json（ビューアの初期視点用）
//
// 何を計算しているか:
//
//	「同じ点が複数の写真のどこに写っているか」を手掛かりに、
//	全カメラの位置・向き・焦点距離と、点の3D座標を同時に求める。
//
// 続きの版（「続きを撮る」で作ったスキャン。base/ に元の版の地図がある）では、ゼロから解かずに
// 元の版の結果を出発点にする:
//
//	database.db ← base/database.db の複製（引き継いだ写真の特徴点とマッチが入っている）
//	(1) 特徴抽出:   DB に既にある写真は COLMAP が飛ばす（IMAGE_EXISTS）。今回の写真だけを処理する。
//	(2) マッチング: マッチ済みの組は飛ばす。今回の写真と全写真の組だけを調べる。
//	(3) マッピング: base/model（元の版のカメラと3D点）から始め、今回の写真を1枚ずつ登録する。
//	                元の写真のカメラ位置は固定するので、地図の座標系が版をまたいで変わらない。
//
// これは VPS（Visual Positioning System）の位置合わせと同じ処理を、写真の束に対して行うことに当たる。
func (p *Pipeline) runSfM(ctx context.Context, job Job, rep Reporter) (string, error) {
	ws := job.WS
	logPath := ws.LogPath(scan.StepSfM)

	resume := ws.HasBase()
	var inherited map[string]bool
	if resume {
		var err error
		if inherited, err = ws.BaseFrames(); err != nil {
			return "", err
		}
		// 引き継いだ DB は元の版とハードリンクで共有している。COLMAP はその場で書き換えるので、
		// 元の版を壊さないよう複製して使う。やり直しのときも、ここで元の状態から作り直される。
		if err := workspace.CopyFile(ws.BaseDatabasePath(), ws.DatabasePath()); err != nil {
			return "", fmt.Errorf("元の版の地図を複製できません: %w", err)
		}
	}

	total, sameSize, err := inspectImages(ws.ImagesDir())
	if err != nil {
		return "", err
	}

	// (1) 特徴抽出: 各写真から「角や模様など、他の写真でも見つけやすい点」（SIFT 特徴点）を取り出す。
	//   入力: images/ の全画像    出力: database.db に特徴点の位置と記述子（128次元のベクトル）
	//   camera_model SIMPLE_RADIAL: 焦点距離 f、中心 (cx,cy)、歪み係数 k の4つでレンズを表す。
	//     スマホの写真には十分で、OpenSplat も対応している。
	//   single_camera: 全写真の寸法が同じなら「同じカメラ・同じズーム」とみなして f,cx,cy,k を共有する。
	//     推定する未知数が減って安定する。寸法が混ざっていたら別々のカメラとして推定する。
	//   use_gpu 0: GPU 版 SIFT は OpenGL の画面を必要とするので、サーバーでは CPU 版を使う。
	args := []string{"feature_extractor",
		"--database_path", ws.DatabasePath(),
		"--image_path", ws.ImagesDir(),
		"--ImageReader.camera_model", "SIMPLE_RADIAL",
		"--ImageReader.single_camera", boolArg(sameSize),
		"--FeatureExtraction.use_gpu", boolArg(p.cfg.ColmapUseGPU),
		"--FeatureExtraction.max_image_size", strconv.Itoa(p.cfg.ColmapMaxImageSize),
	}
	args = p.withThreads(args, "--FeatureExtraction.num_threads")
	if err := p.colmap(ctx, logPath, p.cfg.SfMTimeout, args...); err != nil {
		return "", err
	}

	// (2) マッチング: 写真の全ペアについて、記述子が似ている特徴点どうしを対応づける。
	//   入力: database.db の特徴点    出力: database.db に「画像Aの点i ↔ 画像Bの点j」の対応
	//   exhaustive（総当たり）は N 枚で N(N-1)/2 ペアを調べる。数十枚なら確実で速い。
	//   数百枚を超えたら sequential_matcher や vocab_tree_matcher に替える。
	//   対応はエピポーラ幾何（2台のカメラの位置関係が満たすべき式）で検証され、誤対応が除かれる。
	args = []string{"exhaustive_matcher",
		"--database_path", ws.DatabasePath(),
		"--FeatureMatching.use_gpu", boolArg(p.cfg.ColmapUseGPU),
	}
	args = p.withThreads(args, "--FeatureMatching.num_threads")
	if err := p.colmap(ctx, logPath, p.cfg.SfMTimeout, args...); err != nil {
		return "", err
	}

	// (3) マッピング（インクリメンタル SfM）:
	//   対応の多い2枚から始め、三角測量で3D点を作り、1枚ずつカメラを追加していく。
	//   追加のたびに「再投影誤差（3D点を写真に投影し直したときのずれ）」が最小になるよう、
	//   全カメラと全点をまとめて微調整する（バンドル調整）。
	//   入力: database.db    出力: sparse/0/ に cameras.bin, images.bin, points3D.bin
	//   写真どうしがつながらないと sparse/1/, sparse/2/ ... と複数のモデルに分かれることがある。
	if err := os.MkdirAll(ws.SparseDir(), 0o750); err != nil {
		return "", err
	}
	args = []string{"mapper",
		"--database_path", ws.DatabasePath(),
		"--image_path", ws.ImagesDir(),
	}
	if resume {
		// --input_path: 既存のモデルから続ける。このとき mapper は番号付きの下位ディレクトリを作らず、
		//   --output_path に直接書く（COLMAP 4.2.1 で確認）。
		// fix_existing_frames: 元の写真のカメラ位置を動かさない。地図の座標系が保たれるので、
		//   前の版で位置合わせ（VPS）した結果や、前の版からダウンロードしたデータとそのまま重なる。
		if err := os.MkdirAll(ws.ModelDir(), 0o750); err != nil {
			return "", err
		}
		args = append(args, "--input_path", ws.BaseModelDir(), "--output_path", ws.ModelDir(),
			"--Mapper.fix_existing_frames", "1")
	} else {
		args = append(args, "--output_path", ws.SparseDir())
	}
	args = p.withThreads(args, "--Mapper.num_threads")
	if err := p.colmap(ctx, logPath, p.cfg.SfMTimeout, args...); err != nil {
		// 写真どうしがつながらないと、mapper は「モデルを1つも作れなかった」と出力して
		// 終了コード 1 で終わる（COLMAP 4.2.1 で確認）。最もよくある失敗なので、
		// 「終了コード 1」ではなく、撮り直し方の分かる説明に置き換える。
		if noSparseModel(err) {
			return "", friendly(noModelMessage)
		}
		return "", err
	}

	// (4) 結果の検証: モデルが1つも無い、または登録できた写真が少なすぎるなら失敗にする。
	info, err := p.selectBestModel(ctx, ws, logPath, total)
	if err != nil {
		return "", err
	}
	// 続きの版では、今回の写真が1枚も地図に入らなければ失敗にする。成功にすると、
	// 元の版と同じ3Dデータを時間をかけて作り直すだけになる。
	var addedTotal, added int
	if resume {
		addedTotal = total - len(inherited)
		for _, name := range info.names {
			if !inherited[name] {
				added++
			}
		}
		if added == 0 {
			return "", friendly(noNewImagesMessage)
		}
	}

	// (5) 疎な点群を PLY に書き出す（ビューアでの確認用）。
	sparsePLY := filepath.Join(ws.OutputDir(), "sparse.ply")
	if err := p.colmap(ctx, logPath, p.cfg.SfMTimeout, "model_converter",
		"--input_path", ws.ModelDir(),
		"--output_path", sparsePLY,
		"--output_type", "PLY"); err != nil {
		return "", err
	}
	if err := p.publish(ctx, rep, scan.AssetSparsePLY, sparsePLY, 100, []byte("ply")); err != nil {
		return "", err
	}

	// (6) ビューア用の要約（上方向・中心・カメラ位置）を書き出す。
	scenePath := filepath.Join(ws.OutputDir(), "scene.json")
	if err := WriteSceneInfo(info, scenePath); err != nil {
		return "", err
	}
	if err := p.publish(ctx, rep, scan.AssetSceneJSON, scenePath, 10, []byte("{")); err != nil {
		return "", err
	}

	if resume {
		msg := fmt.Sprintf("今回の%d枚中%d枚を、保存済みの3Dデータの位置に合わせて追加しました。"+
			"全体で%d枚のカメラ位置と%d個の3D点になりました（平均再投影誤差 %.2f px）。",
			addedTotal, added, info.RegisteredImages, info.Points, info.MeanReprojectionError)
		if added < addedTotal {
			msg += "位置を合わせられなかった写真は、以降の処理では使われません。"
		}
		return msg, nil
	}
	msg := fmt.Sprintf("%d枚中%d枚のカメラ位置を推定し、%d個の3D点を得ました（平均再投影誤差 %.2f px）。",
		info.TotalImages, info.RegisteredImages, info.Points, info.MeanReprojectionError)
	if info.RegisteredImages < info.TotalImages {
		msg += "位置を推定できなかった写真は、以降の処理では使われません。"
	}
	return msg, nil
}

// noNewImagesMessage は、続きの版で今回の写真を1枚も地図に合わせられなかったときの説明。
const noNewImagesMessage = "今回の写真を、保存済みの3Dデータの位置に合わせられませんでした。" +
	"前回撮った部分が写るように、前回の撮影位置の近くから撮り始めて、少しずつ範囲を広げてください。" +
	"照明や物の配置が大きく変わっていると一致しにくくなります。"

// noModelMessage は、SfM がモデルを1つも作れなかったときの説明。
const noModelMessage = "写真どうしの対応が見つからず、カメラの位置を推定できませんでした。" +
	"隣り合う写真が7割ほど重なるように、対象のまわりを少しずつ移動しながら撮り直してください。" +
	"無地の壁や光沢のある面、ぶれた写真は対応が取りにくくなります。"

// noSparseModel は、mapper の失敗が「モデルを作れなかった」ことによるものかを返す。
func noSparseModel(err error) bool {
	var exitErr *cmdrun.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return strings.Contains(exitErr.StderrTail, "Failed to create any sparse model") ||
		strings.Contains(exitErr.StderrTail, "No images with matches")
}

// minRegisteredImages は SfM を成功とみなす最小の登録枚数。
// 2枚でも三角測量はできるが、メッシュや 3DGS にはまず足りない。
const minRegisteredImages = 3

// selectBestModel は sparse/ にあるモデルのうち、登録画像が最も多いものを sparse/0 にして要約を返す。
//
// なぜ必要か: mapper は写真が1つながりにならないとき複数のモデルを出力する。
// 後段（OpenSplat など）は sparse/0 を読むので、いちばん大きいモデルを 0 番に置く。
func (p *Pipeline) selectBestModel(ctx context.Context, ws *workspace.Scan, logPath string, total int) (*SceneInfo, error) {
	entries, err := os.ReadDir(ws.SparseDir())
	if err != nil {
		return nil, err
	}
	var best *SceneInfo
	bestName := ""
	for _, e := range entries {
		// mapper が作るモデルのディレクトリ名は 0, 1, 2 ... の数字。
		if _, convErr := strconv.Atoi(e.Name()); !e.IsDir() || convErr != nil {
			continue
		}
		// バイナリのモデルをテキストに変換してから読む。テキスト形式は COLMAP のドキュメントに
		// 仕様があり、人が開いて確かめられる（学習用にも都合がよい）。
		txtDir := filepath.Join(ws.ModelTextDir(), e.Name())
		if err := os.MkdirAll(txtDir, 0o750); err != nil {
			return nil, err
		}
		if err := p.colmap(ctx, logPath, p.cfg.SfMTimeout, "model_converter",
			"--input_path", filepath.Join(ws.SparseDir(), e.Name()),
			"--output_path", txtDir,
			"--output_type", "TXT"); err != nil {
			return nil, err
		}
		info, err := ReadSceneInfo(txtDir, total)
		if err != nil {
			p.log.Warn("モデルを読めないので候補から外します", "model", e.Name(), "error", err)
			continue
		}
		if best == nil || info.RegisteredImages > best.RegisteredImages {
			best, bestName = info, e.Name()
		}
	}

	if best == nil {
		return nil, friendly(noModelMessage)
	}
	if best.RegisteredImages < minRegisteredImages {
		return nil, friendly(fmt.Sprintf("%d枚中%d枚しかカメラの位置を推定できませんでした。"+
			"写真の重なりが足りないか、ぶれている可能性があります。枚数を増やして撮り直してください。",
			total, best.RegisteredImages))
	}

	if bestName != "0" {
		// いちばん大きいモデルを 0 番にする（元の 0 番は別名で残す）。
		zero := filepath.Join(ws.SparseDir(), "0")
		if _, err := os.Stat(zero); err == nil {
			if err := os.Rename(zero, filepath.Join(ws.SparseDir(), "smaller_0")); err != nil {
				return nil, err
			}
		}
		if err := os.Rename(filepath.Join(ws.SparseDir(), bestName), zero); err != nil {
			return nil, err
		}
	}
	return best, nil
}

// ---------------------------------------------------------------------------
// メッシュ分岐（MVS → Mesh）
// ---------------------------------------------------------------------------

func (p *Pipeline) runMeshBranch(ctx context.Context, job Job, rep Reporter) {
	if !p.cfg.EnableMesh {
		rep.StepFinished(ctx, scan.StepMVS, scan.StepSkipped, "設定でメッシュ生成が無効になっています。")
		rep.StepFinished(ctx, scan.StepMesh, scan.StepSkipped, "設定でメッシュ生成が無効になっています。")
		return
	}

	dense := p.DenseMesh()
	if dense {
		if err := p.step(ctx, rep, scan.StepMVS, func() (string, error) { return p.runDenseMVS(ctx, job) }); err != nil {
			if ctx.Err() == nil {
				rep.StepFinished(ctx, scan.StepMesh, scan.StepSkipped, "MVS が失敗したため実行していません。")
			}
			return
		}
	} else {
		// 「実行しなかった」ことと理由を隠さず記録する。
		rep.StepFinished(ctx, scan.StepMVS, scan.StepSkipped,
			"このサーバーの COLMAP は GPU（CUDA）に対応していないため、密な復元（patch_match_stereo）を実行できません。"+
				"代わりに、疎な点群から簡易メッシュを作ります。")
	}

	// メッシュ分岐の失敗は 3DGS 分岐を止めない。エラーは step が Reporter に伝え済み。
	_ = p.step(ctx, rep, scan.StepMesh, func() (string, error) { return p.runMesh(ctx, job, rep, dense) })
}

// undistort は「レンズの歪みを取り除いた画像」と、それに対応するカメラ（歪みなしの PINHOLE）を作る。
//
// 入力: images/ と sparse/0/
// 出力: dense/images/（歪み補正済みの画像）、dense/sparse/（PINHOLE に直したモデル）、dense/stereo/（MVS の設定）
//
// なぜ必要か: MVS もテクスチャ貼りも「3D点を画像に投影すると何画素目か」を大量に計算する。
// 歪みがあると直線が曲がって写るので投影の式が複雑になる。先に画像の方をまっすぐに直しておけば、
// 以降は単純なピンホールカメラの式（u = f·X/Z + cx）だけで済む。この処理自体は CPU で動く。
func (p *Pipeline) undistort(ctx context.Context, ws *workspace.Scan, logPath string, timeout time.Duration) error {
	args := []string{"image_undistorter",
		"--image_path", ws.ImagesDir(),
		"--input_path", ws.ModelDir(),
		"--output_path", ws.DenseDir(),
		"--output_type", "COLMAP",
		"--max_image_size", strconv.Itoa(p.cfg.ColmapMaxImageSize),
	}
	args = p.withThreads(args, "--num_threads")
	return p.colmap(ctx, logPath, timeout, args...)
}

// runDenseMVS は Multi-View Stereo で密な点群を作る（CUDA 対応の COLMAP が必要）。
//
// 入力: images/ と sparse/0/
// 出力: dense/fused.ply（色と法線つきの密な点群）
//
// 注意: このコードパスは GPU の無い環境では実行できない。開発時点（2026-11-16）では
// 偽の Runner を使った単体テストでコマンドの順番と引数だけを確認しており、実機では未検証。
func (p *Pipeline) runDenseMVS(ctx context.Context, job Job) (string, error) {
	ws := job.WS
	logPath := ws.LogPath(scan.StepMVS)

	if err := p.undistort(ctx, ws, logPath, p.cfg.MVSTimeout); err != nil {
		return "", err
	}

	// PatchMatch ステレオ: 各画像の各画素について「奥行き」と「面の向き（法線）」を推定する。
	//   ある画素の奥行きを仮定すると、その3D点が隣の写真のどこに写るかが決まる。
	//   両者の周囲の模様がよく一致する（NCC が高い）奥行きほど正しい、と考えて探索する。
	//   geom_consistency true: 隣の写真の奥行きマップとも矛盾しないことを条件に加える（遅いが正確）。
	//   入力: dense/images, dense/sparse    出力: dense/stereo/depth_maps, normal_maps
	if err := p.colmap(ctx, logPath, p.cfg.MVSTimeout, "patch_match_stereo",
		"--workspace_path", ws.DenseDir(),
		"--workspace_format", "COLMAP",
		"--PatchMatchStereo.geom_consistency", "true"); err != nil {
		return "", err
	}

	// ステレオ融合: 画像ごとの奥行きマップを3D空間で突き合わせ、複数の画像で整合する点だけを残して
	// 1つの密な点群にまとめる。
	//   入力: dense/stereo/    出力: dense/fused.ply
	fused := filepath.Join(ws.DenseDir(), "fused.ply")
	args := []string{"stereo_fusion",
		"--workspace_path", ws.DenseDir(),
		"--workspace_format", "COLMAP",
		"--input_type", "geometric",
		"--output_path", fused,
	}
	args = p.withThreads(args, "--StereoFusion.num_threads")
	if err := p.colmap(ctx, logPath, p.cfg.MVSTimeout, args...); err != nil {
		return "", err
	}

	if _, err := checkFile(fused, 1024, []byte("ply")); err != nil {
		return "", friendly("密な点群を作れませんでした。写真の重なりや枚数が足りない可能性があります。")
	}
	n, _ := PLYVertexCount(fused)
	return fmt.Sprintf("%d点の密な点群を作りました。", n), nil
}

// runMesh は点群から三角形メッシュを作り、テクスチャを貼り、GLB にする。
//
// 入力: dense=true なら dense/fused.ply（密な点群）、false なら sparse/0（疎な点群）
// 出力: output/mesh.glb
func (p *Pipeline) runMesh(ctx context.Context, job Job, rep Reporter, dense bool) (string, error) {
	ws := job.WS
	logPath := ws.LogPath(scan.StepMesh)
	meshed := filepath.Join(ws.DenseDir(), "meshed.ply")

	var kind string
	if dense {
		kind = "密な点群から"
		// ポアソン表面再構成: 点とその法線から「内側か外側か」を表す滑らかな関数を解き、
		// その境界面を三角形で取り出す。穴の無い滑らかな面になるが、点の無い所にも面を張る。
		//   入力: dense/fused.ply（法線つき）    出力: dense/meshed.ply
		args := []string{"poisson_mesher", "--input_path", filepath.Join(ws.DenseDir(), "fused.ply"), "--output_path", meshed}
		args = p.withThreads(args, "--PoissonMeshing.num_threads")
		if err := p.colmap(ctx, logPath, p.cfg.MeshTimeout, args...); err != nil {
			return "", err
		}
	} else {
		kind = "疎な点群からの簡易版"
		// 密な復元を省いた場合は、テクスチャ貼りに必要な歪み補正をここで行う。
		if err := p.undistort(ctx, ws, logPath, p.cfg.MeshTimeout); err != nil {
			return "", err
		}
		// ドロネー・メッシュ化（疎）: 3D点を四面体で埋め尽くし（ドロネー四面体分割）、
		// 「カメラからその点が見えた」という視線の情報を使って各四面体を内側/外側に分け（グラフカット）、
		// その境界を面として取り出す。点が数千個しかないので形は粗い。
		//   入力: dense/sparse（疎なモデル）    出力: dense/meshed.ply
		args := []string{"delaunay_mesher",
			"--input_path", filepath.Join(ws.DenseDir(), "sparse"),
			"--input_type", "sparse",
			"--output_path", meshed,
		}
		args = p.withThreads(args, "--DelaunayMeshing.num_threads")
		if err := p.colmap(ctx, logPath, p.cfg.MeshTimeout, args...); err != nil {
			return "", err
		}
	}
	if _, err := checkFile(meshed, 200, []byte("ply")); err != nil {
		return "", friendly("点群からメッシュ（面）を作れませんでした。3D点が少なすぎる可能性があります。")
	}

	// テクスチャ貼り: 各三角形について「その面を最も正面からはっきり写している写真」を選び、
	// その部分を1枚の大きな画像（テクスチャアトラス）に並べる。各面には、アトラス上の
	// どこを貼るかを示す UV 座標が付く。
	//   入力: dense/（歪み補正済みの画像とカメラ）、dense/meshed.ply
	//   出力: dense/textured/mesh.ply（面ごとの UV つき）、dense/textured/texture.png
	texturedDir := filepath.Join(ws.DenseDir(), "textured")
	args := []string{"mesh_texturer",
		"--workspace_path", ws.DenseDir(),
		"--input_path", meshed,
		"--output_path", texturedDir,
	}
	args = p.withThreads(args, "--MeshTextureMapping.num_threads")
	if err := p.colmap(ctx, logPath, p.cfg.MeshTimeout, args...); err != nil {
		return "", err
	}
	texturedPLY := filepath.Join(texturedDir, "mesh.ply")
	if _, err := checkFile(texturedPLY, 200, []byte("ply")); err != nil {
		return "", friendly("メッシュにテクスチャを貼れませんでした。")
	}
	if _, err := checkFile(filepath.Join(texturedDir, "texture.png"), 100, []byte("\x89PNG")); err != nil {
		return "", friendly("テクスチャ画像が生成されませんでした。")
	}

	// PLY → OBJ: gltfpack は PLY を読めないので、Go で OBJ+MTL に書き直す（obj.go）。
	mesh, err := ReadPLYMesh(texturedPLY)
	if err != nil {
		return "", fmt.Errorf("テクスチャ付きメッシュを読めません: %w", err)
	}
	objPath := filepath.Join(texturedDir, "mesh.obj")
	if err := WriteOBJ(mesh, objPath); err != nil {
		return "", fmt.Errorf("OBJ を書き出せません: %w", err)
	}

	// OBJ → GLB: glTF は Web 向けの標準3D形式。GLB はその「1ファイルにまとめた版」で、
	// 形状・材質・テクスチャ画像がすべて入るので配信しやすい。
	//   -noq: 頂点の量子化（座標を整数に丸めて小さくする拡張）を使わない。
	//         ファイルは少し大きくなるが、拡張に対応していないソフトでも読める。
	glb := filepath.Join(ws.OutputDir(), "mesh.glb")
	if _, err := p.run.Run(ctx, cmdrun.Command{
		Name:    p.cfg.GltfpackBin,
		Args:    []string{"-i", objPath, "-o", glb, "-noq"},
		Timeout: p.cfg.MeshTimeout,
		LogPath: logPath,
	}); err != nil {
		return "", err
	}
	// GLB の先頭4バイトは必ず "glTF"。
	if err := p.publish(ctx, rep, scan.AssetMeshGLB, glb, 1024, []byte("glTF")); err != nil {
		return "", err
	}
	return fmt.Sprintf("頂点%d・面%dのメッシュ（%s）にテクスチャを貼りました。", len(mesh.Vertices), len(mesh.Faces), kind), nil
}

// ---------------------------------------------------------------------------
// 3DGS 分岐（Splat → SOG）
// ---------------------------------------------------------------------------

func (p *Pipeline) runSplatBranch(ctx context.Context, job Job, rep Reporter) {
	if !p.cfg.EnableSplat {
		rep.StepFinished(ctx, scan.StepSplat, scan.StepSkipped, "設定で 3DGS 生成が無効になっています。")
		rep.StepFinished(ctx, scan.StepSOG, scan.StepSkipped, "設定で 3DGS 生成が無効になっています。")
		return
	}
	if err := p.step(ctx, rep, scan.StepSplat, func() (string, error) { return p.runSplat(ctx, job, rep) }); err != nil {
		if ctx.Err() == nil {
			rep.StepFinished(ctx, scan.StepSOG, scan.StepSkipped, "3DGS の学習が失敗したため実行していません。")
		}
		return
	}
	// SOG への変換が失敗しても、PLY は既に登録済みなのでビューアは PLY で表示できる。
	_ = p.step(ctx, rep, scan.StepSOG, func() (string, error) { return p.runSOG(ctx, job, rep) })
}

// runSplat は OpenSplat で 3D Gaussian Splatting を学習する。
//
// 入力: 作業ディレクトリ（直下の images/ と sparse/0/ を OpenSplat が読む）
// 出力: output/splat.ply
//
// 何を計算しているか:
//
//	シーンを「半透明の色つき楕円体（3次元ガウス分布）」の集まりで表す。SfM の疎な点を初期値にして、
//	「いまのガウシアンを各カメラから描画した絵」と「実際の写真」の差が小さくなるように、
//	各ガウシアンの位置・大きさ・向き・色・不透明度を勾配降下法で少しずつ直していく。
//	足りない所は分割・複製で増やし、ほぼ透明なものは消す。メッシュ（面）は作らない。
func (p *Pipeline) runSplat(ctx context.Context, job Job, rep Reporter) (string, error) {
	ws := job.WS
	out := filepath.Join(ws.OutputDir(), "splat.ply")

	// -n: 学習の反復回数。-d: 画像を 1/d に縮小して学習（速くなるが細部は甘くなる）。
	// -o: 出力先。指定しないと入力の隣に書かれるので、必ず output/ を明示する。
	args := []string{ws.Dir(),
		"-n", strconv.Itoa(p.cfg.SplatIterations),
		"-d", strconv.FormatFloat(p.cfg.SplatDownscale, 'g', -1, 64),
		"-o", out,
	}
	if p.cfg.SplatForceCPU {
		// --cpu: GPU を探さず CPU で実行する。OpenSplat の README によれば GPU の約100倍遅い。
		args = append(args, "--cpu")
	}
	cmd := cmdrun.Command{
		Name:    p.cfg.OpenSplatBin,
		Args:    args,
		Dir:     ws.Dir(),
		Timeout: p.cfg.SplatTimeout,
		LogPath: ws.LogPath(scan.StepSplat),
	}
	if p.cfg.NumThreads > 0 {
		// libtorch（PyTorch の C++ 版）は OpenMP でスレッド数を決める。
		cmd.Env = []string{"OMP_NUM_THREADS=" + strconv.Itoa(p.cfg.NumThreads)}
	}
	if _, err := p.run.Run(ctx, cmd); err != nil {
		return "", err
	}
	if err := p.publish(ctx, rep, scan.AssetGaussianPLY, out, 1024, []byte("ply")); err != nil {
		return "", err
	}
	n, _ := PLYVertexCount(out)
	device := "GPU"
	if p.cfg.SplatForceCPU {
		device = "CPU"
	}
	return fmt.Sprintf("%d個のガウシアンを学習しました（反復%d回、%s）。", n, p.cfg.SplatIterations, device), nil
}

// runSOG は 3DGS の PLY を SOG 形式に変換する。
//
// 入力: output/splat.ply    出力: output/scene.sog
//
// SOG（Spatially Ordered Gaussians）は PlayCanvas の配信用形式。ガウシアンの各属性を量子化して
// WebP 画像に詰め、ZIP 1ファイルにまとめる。PLY の 1/15〜1/20 程度になる（不可逆圧縮）。
func (p *Pipeline) runSOG(ctx context.Context, job Job, rep Reporter) (string, error) {
	ws := job.WS
	in := filepath.Join(ws.OutputDir(), "splat.ply")
	out := filepath.Join(ws.OutputDir(), "scene.sog")

	// -w: 出力ファイルがあれば上書き（やり直しのとき用）。
	// -g cpu: 圧縮の中で唯一 GPU を使う処理（球面調和係数の k-means）を CPU で行う。
	//         公式ドキュメントに「GPU が無くても動くが 5〜10 倍遅い」とある。
	if _, err := p.run.Run(ctx, cmdrun.Command{
		Name:    p.cfg.SplatTransformBin,
		Args:    []string{"-w", "-g", "cpu", in, out},
		Dir:     ws.Dir(),
		Timeout: p.cfg.SOGTimeout,
		LogPath: ws.LogPath(scan.StepSOG),
	}); err != nil {
		return "", err
	}
	// バンドル版の .sog は ZIP なので、先頭は "PK"。
	if err := p.publish(ctx, rep, scan.AssetGaussianSOG, out, 100, []byte("PK")); err != nil {
		return "", err
	}
	inSize, _ := fileSize(in)
	outSize, _ := fileSize(out)
	return fmt.Sprintf("%s → %s に圧縮しました。", humanBytes(inSize), humanBytes(outSize)), nil
}

// ---------------------------------------------------------------------------
// 補助
// ---------------------------------------------------------------------------

// colmap は colmap のサブコマンドを1つ実行する。
func (p *Pipeline) colmap(ctx context.Context, logPath string, timeout time.Duration, args ...string) error {
	_, err := p.run.Run(ctx, cmdrun.Command{
		Name:    p.cfg.ColmapBin,
		Args:    args,
		Timeout: timeout,
		LogPath: logPath,
	})
	return err
}

// withThreads は NumThreads が指定されているときだけ「<flag> N」を足す。
func (p *Pipeline) withThreads(args []string, flag string) []string {
	if p.cfg.NumThreads > 0 {
		return append(args, flag, strconv.Itoa(p.cfg.NumThreads))
	}
	return args
}

// publish は成果物を検証し、通ったものだけを Reporter に登録する。
//
// 「コマンドは 0 で終わったが、出力が無い／空／別物」という失敗を成功にしないための関門。
func (p *Pipeline) publish(ctx context.Context, rep Reporter, kind scan.AssetKind, path string, minBytes int64, magic []byte) error {
	size, err := checkFile(path, minBytes, magic)
	if err != nil {
		return fmt.Errorf("成果物 %s の検証に失敗: %w", kind, err)
	}
	rep.AssetReady(ctx, kind, path, size)
	return nil
}

// checkFile はファイルが存在し、minBytes 以上で、先頭が magic で始まることを確かめる。
func checkFile(path string, minBytes int64, magic []byte) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("出力ファイルがありません: %s", filepath.Base(path))
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, fmt.Errorf("通常のファイルではありません: %s", filepath.Base(path))
	}
	if st.Size() < minBytes {
		return 0, fmt.Errorf("出力ファイルが小さすぎます: %s（%dバイト）", filepath.Base(path), st.Size())
	}
	head := make([]byte, len(magic))
	if _, err := f.ReadAt(head, 0); err != nil || string(head) != string(magic) {
		return 0, fmt.Errorf("出力ファイルの形式が想定と違います: %s", filepath.Base(path))
	}
	return st.Size(), nil
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func boolArg(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// friendlyError は、そのまま画面に出してよい日本語の説明を持つエラー。
type friendlyError struct{ msg string }

func (e *friendlyError) Error() string { return e.msg }

func friendly(msg string) error { return &friendlyError{msg: msg} }

// userMessage はエラーを利用者向けの説明に直す。技術的な詳細はログファイルに残っている。
func userMessage(err error) string {
	var fe *friendlyError
	if errors.As(err, &fe) {
		return fe.msg
	}
	if errors.Is(err, cmdrun.ErrTimeout) {
		return "制限時間内に終わりませんでした。写真の枚数を減らすと終わる可能性があります。"
	}
	var exitErr *cmdrun.ExitError
	if errors.As(err, &exitErr) {
		tool := filepath.Base(exitErr.Command)
		msg := fmt.Sprintf("%s がエラーで終了しました（終了コード %d）。", tool, exitErr.ExitCode)
		if exitErr.Signal != "" {
			msg = fmt.Sprintf("%s が異常終了しました（%s）。", tool, exitErr.Signal)
		}
		if hint := exitHint(exitErr); hint != "" {
			msg += hint
		} else {
			msg += "詳細は処理ログを確認してください。"
		}
		return msg
	}
	return "処理中にエラーが発生しました: " + err.Error()
}

// exitHint は、よくある失敗に対して原因の見当を付ける一言を返す。
//
// 止まり方（終了コードとシグナル）と、標準エラー出力の文言を手掛かりにする。
// 確実に分かるわけではないので「可能性があります」と書き、決めつけない。
func exitHint(e *cmdrun.ExitError) string {
	tail := strings.ToLower(e.StderrTail)
	switch {
	case strings.Contains(tail, "no space left"):
		return "サーバーのディスクが一杯です。"
	case strings.Contains(tail, "requires cuda") || strings.Contains(tail, "cuda"):
		return "この処理には NVIDIA GPU（CUDA）が必要ですが、サーバーにありません。"
	// SIGKILL は外からの強制終了。コンテナではメモリ上限の超過（OOM Kill）が典型。
	// 終了コード 137 は 128 + 9（SIGKILL）で、シェル経由で同じことが起きたときの値。
	case e.Signal == "SIGKILL" || e.ExitCode == 137 || strings.Contains(tail, "out of memory") || strings.Contains(tail, "bad_alloc"):
		return "メモリ不足の可能性があります。写真の枚数か解像度を減らしてください。"
	// SIGABRT / SIGSEGV はツール自身の異常終了。入力が乏しい（点や写真が少なすぎる）ときに起きやすい。
	case e.Signal != "":
		return "ツールが処理を続けられませんでした。位置を推定できた写真や3D点が少なすぎる可能性があります。" +
			"写真の重なりを増やして撮り直すと改善することがあります。"
	}
	return ""
}
