// Package vps は VPS（Visual Positioning System）の位置合わせを担当する。
//
// 「いま撮った写真1枚が、保存済みの3Dデータ（地図）のどこから、どちらを向いて撮ったものか」を求める。
// GPS の代わりに「写っている景色」で位置を決めるので、屋内でも使え、答えは地図と同じ座標系で出る。
// 「続きを撮る」画面で、前回の撮影範囲に対していまどこに居るかを示すのに使う。
//
// 地図は SfM の結果そのもの（新しく作る必要がない）:
//
//	database.db  地図の各写真の SIFT 特徴点
//	sparse/0/    地図の各写真のカメラの位置と、3D点（どの写真のどの特徴点に写っているか付き）
//
// 手順（すべて COLMAP の CLI。地図そのものは書き換えず、一時ディレクトリで作業する）:
//
//	(1) 特徴抽出    写真の SIFT 特徴点を、地図の DB の複製に足す
//	(2) マッチング  地図の写真（最大 MaxPairs 枚）と特徴点を突き合わせる
//	                → 「写真の2D点 ↔ 地図の写真の2D点 ↔ その点の3D座標」という対応ができる
//	(3) 登録        2D-3D の対応から、カメラの位置と向きを解く（PnP を RANSAC で頑健に解く）。
//	                image_registrator が、SfM で写真を1枚ずつ足すときと同じ手順で1枚だけ追加する
//	(4) 読み出し    追加後のモデルをテキストに変換し、写真の位置と向きを取り出す
//
// 対応が足りない（別の場所の写真、照明や配置が大きく違う、ぶれている）と (3) で登録されない。
// そのときは「位置が分からなかった」と答える（エラーではない）。
package vps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/cmdrun"
	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// ErrBusy は、別の位置合わせを処理中で受け付けられないことを表す。
var ErrBusy = errors.New("別の位置合わせを処理中です")

// ErrTimeout は、制限時間内に位置合わせが終わらなかったことを表す。
var ErrTimeout = errors.New("位置合わせが制限時間内に終わりませんでした")

// queryBase は一時ディレクトリでの写真の名前（拡張子を除く）。地図の写真は frame_0001.jpg のような
// 名前なので重ならない。COLMAP は DB の中で写真を名前で見分けるため、重なってはいけない。
const queryBase = "vps_query"

// Query は位置を知りたい写真1枚。
type Query struct {
	// Path は写真のファイル。位置合わせの作業ディレクトリに移すので、呼び出し後は残らない。
	Path string
	// Width と Height は写真の寸法。地図のカメラと同じなら、そのカメラの焦点距離などを使い回す。
	Width, Height int
}

// Result は位置合わせの結果。
type Result struct {
	// Localized は位置が分かったか。false なら他の項目は意味を持たない。
	Localized bool
	// Position はカメラの位置、Forward はレンズの向き（単位ベクトル）。どちらも地図の座標系。
	Position [3]float64
	Forward  [3]float64
	// Matches は、写真の特徴点のうち地図の3D点に対応づいた数。多いほど確か。
	Matches int
	// ComparedImages は特徴点を突き合わせた地図の写真の枚数。
	ComparedImages int
	// MapImages は地図に入っている写真の枚数。
	MapImages int
}

// Localizer は位置合わせを実行する。同時に1件だけ動かす。
type Localizer struct {
	cfg  config.Pipeline
	run  cmdrun.Runner
	root *workspace.Root
	log  *slog.Logger
	// slot は同時実行を1件に絞る席。COLMAP は全コアを使うので、並べても速くならない。
	slot chan struct{}

	// Timeout は1件の制限時間。前段の Cloudflare は約100秒で応答を打ち切るので、それより短くする。
	Timeout time.Duration
	// MaxPairs は突き合わせる地図の写真の上限。多いほど見つかりやすいが遅くなる。
	MaxPairs int
}

// New は Localizer を作る。cfg からは COLMAP の場所と特徴抽出の設定を使う（復元と同じ設定にする）。
func New(cfg config.Pipeline, run cmdrun.Runner, root *workspace.Root, log *slog.Logger) *Localizer {
	if log == nil {
		log = slog.Default()
	}
	return &Localizer{
		cfg: cfg, run: run, root: root, log: log,
		slot:     make(chan struct{}, 1),
		Timeout:  80 * time.Second,
		MaxPairs: 60,
	}
}

// Localize は写真 q を、スキャン m の地図に対して位置合わせする。
//
// 出力: 位置が分かれば Localized=true の結果。分からなければ Localized=false（エラーではない）。
// 副作用: 一時ディレクトリを作って消す。q.Path は一時ディレクトリに移される。
// 失敗条件: 別の位置合わせを処理中（ErrBusy）、時間切れ（ErrTimeout）、COLMAP の異常終了など。
func (l *Localizer) Localize(ctx context.Context, m *workspace.Scan, q Query) (Result, error) {
	select {
	case l.slot <- struct{}{}:
		defer func() { <-l.slot }()
	default:
		return Result{}, ErrBusy
	}
	if !m.HasMap() {
		return Result{}, errors.New("このスキャンには位置合わせに使える地図がありません")
	}

	runCtx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	dir, err := l.root.NewTemp("vps-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	res, err := l.localize(runCtx, dir, m, q)
	if err != nil && ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return Result{}, ErrTimeout
	}
	return res, err
}

func (l *Localizer) localize(ctx context.Context, dir string, m *workspace.Scan, q Query) (Result, error) {
	var res Result
	logPath := filepath.Join(dir, "localize.log")

	// 地図の中身（写真の一覧とカメラ）を読む。バイナリ形式をテキストに変換してから読む。
	mapTxt := filepath.Join(dir, "map")
	if err := os.MkdirAll(mapTxt, 0o750); err != nil {
		return res, err
	}
	if err := l.colmap(ctx, logPath, "model_converter",
		"--input_path", m.ModelDir(), "--output_path", mapTxt, "--output_type", "TXT"); err != nil {
		return res, err
	}
	mapImages, err := reconstruction.ReadImagePoses(filepath.Join(mapTxt, "images.txt"), false)
	if err != nil {
		return res, fmt.Errorf("地図の写真を読めません: %w", err)
	}
	cameras, err := reconstruction.ReadCameras(filepath.Join(mapTxt, "cameras.txt"))
	if err != nil {
		return res, fmt.Errorf("地図のカメラを読めません: %w", err)
	}
	res.MapImages = len(mapImages)
	if len(mapImages) == 0 {
		return res, errors.New("地図に写真が1枚もありません")
	}

	// 地図の DB を複製して使う。COLMAP は DB をその場で書き換えるので、本物には触らない。
	db := filepath.Join(dir, "database.db")
	if err := workspace.CopyFile(m.DatabasePath(), db); err != nil {
		return res, err
	}
	imagesDir := filepath.Join(dir, "images")
	if err := os.MkdirAll(imagesDir, 0o750); err != nil {
		return res, err
	}
	name := queryBase + strings.ToLower(filepath.Ext(q.Path))
	if err := moveFile(q.Path, filepath.Join(imagesDir, name)); err != nil {
		return res, err
	}

	// (1) 特徴抽出。image_path には今回の写真しか置かないので、抽出されるのはこの1枚だけ。
	//   地図のカメラと寸法が同じなら同じ端末で撮った可能性が高いので、そのカメラ（焦点距離・歪み）を
	//   使い回す（existing_camera_id）。未知数が減り、少ない対応でも位置が決まりやすくなる。
	//   寸法が違えば新しいカメラとし、焦点距離は登録のときに推定させる。
	args := []string{"feature_extractor",
		"--database_path", db,
		"--image_path", imagesDir,
		"--ImageReader.camera_model", "SIMPLE_RADIAL",
		"--FeatureExtraction.use_gpu", boolArg(l.cfg.ColmapUseGPU),
		"--FeatureExtraction.max_image_size", strconv.Itoa(l.cfg.ColmapMaxImageSize),
	}
	if id, ok := sameSizeCamera(cameras, q.Width, q.Height); ok {
		args = append(args, "--ImageReader.existing_camera_id", strconv.Itoa(id))
	}
	args = l.withThreads(args, "--FeatureExtraction.num_threads")
	if err := l.colmap(ctx, logPath, args...); err != nil {
		return res, err
	}

	// (2) マッチング。突き合わせる地図の写真は、地図全体から等間隔に選ぶ（撮影範囲の全体を見る）。
	compared := samplePairs(mapImages, l.MaxPairs)
	res.ComparedImages = len(compared)
	var pairs strings.Builder
	for _, img := range compared {
		fmt.Fprintf(&pairs, "%s %s\n", name, img.Name)
	}
	pairsPath := filepath.Join(dir, "pairs.txt")
	if err := os.WriteFile(pairsPath, []byte(pairs.String()), 0o640); err != nil {
		return res, err
	}
	args = []string{"matches_importer",
		"--database_path", db,
		"--match_list_path", pairsPath,
		"--match_type", "pairs",
		"--FeatureMatching.use_gpu", boolArg(l.cfg.ColmapUseGPU),
	}
	args = l.withThreads(args, "--FeatureMatching.num_threads")
	if err := l.colmap(ctx, logPath, args...); err != nil {
		return res, err
	}

	// (3) 登録。地図に無い写真（＝今回の1枚）を、地図のモデルに追加する。
	//   対応が足りなければ追加されないだけで、コマンド自体は成功する。
	outModel := filepath.Join(dir, "model")
	if err := os.MkdirAll(outModel, 0o750); err != nil {
		return res, err
	}
	if err := l.colmap(ctx, logPath, "image_registrator",
		"--database_path", db, "--input_path", m.ModelDir(), "--output_path", outModel); err != nil {
		return res, err
	}

	// (4) 読み出し。
	outTxt := filepath.Join(dir, "out")
	if err := os.MkdirAll(outTxt, 0o750); err != nil {
		return res, err
	}
	if err := l.colmap(ctx, logPath, "model_converter",
		"--input_path", outModel, "--output_path", outTxt, "--output_type", "TXT"); err != nil {
		return res, err
	}
	poses, err := reconstruction.ReadImagePoses(filepath.Join(outTxt, "images.txt"), true)
	if err != nil {
		return res, fmt.Errorf("位置合わせの結果を読めません: %w", err)
	}
	for _, p := range poses {
		if p.Name == name {
			res.Localized = true
			res.Position, res.Forward, res.Matches = p.Center, p.Forward, p.Observations
		}
	}
	return res, nil
}

// sameSizeCamera は寸法が同じ地図のカメラの ID を返す。
func sameSizeCamera(cameras []reconstruction.Camera, w, h int) (int, bool) {
	for _, c := range cameras {
		if c.Width == w && c.Height == h {
			return c.ID, true
		}
	}
	return 0, false
}

// samplePairs は地図の写真から最大 n 枚を、ID の順に等間隔で選ぶ。
// ID はおおむね撮影（受付）順なので、撮影範囲の全体から満遍なく選ぶことになる。
func samplePairs(images []reconstruction.ImagePose, n int) []reconstruction.ImagePose {
	sorted := append([]reconstruction.ImagePose(nil), images...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	if n <= 0 || len(sorted) <= n {
		return sorted
	}
	out := make([]reconstruction.ImagePose, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, sorted[i*len(sorted)/n])
	}
	return out
}

func (l *Localizer) colmap(ctx context.Context, logPath string, args ...string) error {
	_, err := l.run.Run(ctx, cmdrun.Command{Name: l.cfg.ColmapBin, Args: args, LogPath: logPath})
	if err != nil {
		l.log.Warn("位置合わせの COLMAP が失敗", "command", args[0], "error", err)
	}
	return err
}

func (l *Localizer) withThreads(args []string, flag string) []string {
	if l.cfg.NumThreads > 0 {
		return append(args, flag, strconv.Itoa(l.cfg.NumThreads))
	}
	return args
}

// moveFile は src を dst に移す。同じディスクなら名前の変更だけで済む。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := workspace.CopyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func boolArg(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
