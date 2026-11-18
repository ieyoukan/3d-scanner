// Package workspace は PVC 上の作業ディレクトリ（/data/scans/<id>/）の場所と掃除を担当する。
//
// ディレクトリ構成（COLMAP 公式ドキュメントの「プロジェクトフォルダ」と同じ形）:
//
//	/data/scans/<id>/
//	  images/        入力の写真（frame_0001.jpg ...）
//	  database.db    COLMAP の内部DB（SQLite。特徴点とマッチを保存。PostgreSQL とは別物）
//	  sparse/0/      SfM の結果（cameras.bin / images.bin / points3D.bin）
//	  dense/         歪みを取り除いた画像とカメラ（メッシュ・テクスチャ用の作業場）
//	  output/        配信する成果物（sparse.ply / mesh.glb / splat.ply / scene.sog）
//	  logs/          各ステップの外部CLIの出力
//
// images/ と sparse/0/ を同じ親の下に置くのは、OpenSplat が「プロジェクトのルート」を1つ受け取り、
// その直下の images/ と sparse/0/ を読むため。
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// ErrOutsideRoot は、求めたパスがスキャンの作業ディレクトリの外を指していることを表す。
var ErrOutsideRoot = errors.New("作業ディレクトリの外を指すパスです")

// Root は全スキャンの作業ディレクトリの親（<DATA_DIR>/scans）を表す。
type Root struct {
	dir string
}

// New は dataDir の下に scans/ を用意して Root を返す。
//
// 副作用: ディレクトリを作る。
// 失敗条件: 作れない・書き込めない（PVC が未マウント、権限不足など）。
// 起動時に呼ぶことで、ストレージの問題を最初のアップロードより前に見つけられる。
func New(dataDir string) (*Root, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(abs, "scans")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("作業ディレクトリ %s を作れません: %w", dir, err)
	}
	return &Root{dir: dir}, nil
}

// Dir は scans/ の絶対パスを返す。
func (r *Root) Dir() string { return r.dir }

// Scan は1スキャン分の作業ディレクトリを表す。
type Scan struct {
	dir string
}

// Scan は id の作業ディレクトリを返す（まだ作らない）。
//
// 失敗条件: id が UUID 形式でない。
// ここで形式を確かめるのが、パストラバーサル対策の1つ目の関門。id は URL から来ることがあり、
// "../" のような文字列をそのまま filepath.Join に渡すと scans/ の外に出てしまう。
func (r *Root) Scan(id string) (*Scan, error) {
	if !scan.ValidID(id) {
		return nil, fmt.Errorf("スキャンIDの形式が不正です: %w", ErrOutsideRoot)
	}
	return &Scan{dir: filepath.Join(r.dir, id)}, nil
}

// Open は任意のディレクトリを作業ディレクトリとして開く（output/ と logs/ を用意する）。
//
// サーバーは使わない。運用者が自分でパスを指定する単体実行ツール（cmd/reconstruct）専用。
// サーバー経由の入力は必ず Root.Scan（ID の形式検査つき）を通すこと。
func Open(dir string) (*Scan, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	s := &Scan{dir: abs}
	if fi, err := os.Stat(s.ImagesDir()); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s がありません。作業ディレクトリの直下に images/ を作り、写真を置いてください", s.ImagesDir())
	}
	for _, d := range []string{s.OutputDir(), s.LogsDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Dir はスキャンの作業ディレクトリ（＝COLMAP/OpenSplat のプロジェクトルート）を返す。
func (s *Scan) Dir() string { return s.dir }

// よく使う場所。文字列の連結を各所に散らさないための小さなメソッド群。
func (s *Scan) ImagesDir() string    { return filepath.Join(s.dir, "images") }
func (s *Scan) DatabasePath() string { return filepath.Join(s.dir, "database.db") }
func (s *Scan) SparseDir() string    { return filepath.Join(s.dir, "sparse") }
func (s *Scan) DenseDir() string     { return filepath.Join(s.dir, "dense") }
func (s *Scan) OutputDir() string    { return filepath.Join(s.dir, "output") }
func (s *Scan) LogsDir() string      { return filepath.Join(s.dir, "logs") }

// ThumbnailPath はギャラリー用の縮小画像（受付時に1枚目の写真から作る）の場所を返す。
// 作業ディレクトリの直下に置くので、やり直し（ResetDerived）では消えない。
func (s *Scan) ThumbnailPath() string { return filepath.Join(s.dir, "thumbnail.jpg") }

// ModelDir は採用した SfM モデル（sparse/0）の場所を返す。
// mapper は sparse/ の下に 0, 1, ... とモデルを書く。パイプラインは最も大きいものを 0 番に置く。
func (s *Scan) ModelDir() string { return filepath.Join(s.dir, "sparse", "0") }

// ModelTextDir は SfM モデルをテキスト形式に変換したものの置き場を返す（要約の計算用）。
func (s *Scan) ModelTextDir() string { return filepath.Join(s.dir, "sparse_txt") }

// LogPath はステップのログファイルの場所を返す。
func (s *Scan) LogPath(step scan.StepName) string {
	return filepath.Join(s.LogsDir(), string(step)+".log")
}

// Create は入力用のディレクトリを作る（アップロード受付時に呼ぶ）。
func (s *Scan) Create() error {
	for _, d := range []string{s.ImagesDir(), s.OutputDir(), s.LogsDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("ディレクトリ %s を作れません: %w", d, err)
		}
	}
	return nil
}

// Resolve は作業ディレクトリからの相対パスを絶対パスに直す。
//
// 入力: DB に保存してある相対パス（例: "output/mesh.glb"）。
// 出力: 絶対パス。
// 失敗条件: 絶対パス、".." で外に出るパス、シンボリックリンクを辿ると外に出るパス。
//
// これがパストラバーサル対策の2つ目の関門。相対パスは自分たちが DB に書いた値だが、
// 「DB の値は常に正しい」と仮定せず、ファイルを開く直前にもう一度確かめる（多層防御）。
func (s *Scan) Resolve(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", ErrOutsideRoot
	}
	// Clean で "a/../b" を "b" に、"./x" を "x" に正規化してから親の中かを見る。
	joined := filepath.Join(s.dir, filepath.Clean(rel))
	if !within(s.dir, joined) {
		return "", ErrOutsideRoot
	}
	// シンボリックリンクの実体も確かめる。外部CLIが作ったファイルがリンクだった場合に、
	// リンク先（例: /etc/passwd）を配信してしまわないようにする。
	// ファイルがまだ無い場合は EvalSymlinks が失敗するので、そのときは文字列の検査だけで返す。
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return joined, nil
		}
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return "", err
	}
	if !within(realRoot, real) {
		return "", ErrOutsideRoot
	}
	return joined, nil
}

// Rel は作業ディレクトリ内の絶対パスを、DB に保存する相対パスに直す。
func (s *Scan) Rel(abs string) (string, error) {
	if !within(s.dir, abs) {
		return "", ErrOutsideRoot
	}
	rel, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return "", err
	}
	// DB には OS によらず "/" 区切りで保存する。
	return filepath.ToSlash(rel), nil
}

// within は path が dir そのもの、または dir の配下かを返す。
// 単純な strings.HasPrefix だと "/data/scans/abc" が "/data/scans/abcdef" の接頭辞として
// 一致してしまうので、区切り文字まで含めて比べる。
func within(dir, path string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// Remove は作業ディレクトリを丸ごと消す。既に無ければ何もしない。
//
// 副作用: 写真・中間ファイル・成果物・ログがすべて消える（元に戻せない）。
// 呼ばれる場面: キャンセル、期限切れ、アップロード検証の失敗。
func (s *Scan) Remove() error {
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("作業ディレクトリ %s を削除できません: %w", s.dir, err)
	}
	return nil
}

// ModTime は作業ディレクトリの最終更新時刻を返す（孤児ディレクトリの古さの判定に使う）。
func (s *Scan) ModTime() (time.Time, error) {
	fi, err := os.Stat(s.dir)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// ResetDerived は入力の写真だけを残し、復元の途中経過と成果物を消す。
// 中断されたジョブを最初からやり直す前に呼ぶ（古い database.db や半端な sparse/ が残っていると、
// COLMAP が前回の続きとして扱い、結果が混ざることがある）。
func (s *Scan) ResetDerived() error {
	for _, p := range []string{s.DatabasePath(), s.DatabasePath() + "-shm", s.DatabasePath() + "-wal",
		s.SparseDir(), s.ModelTextDir(), s.DenseDir(), s.OutputDir(), s.LogsDir()} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("%s を削除できません: %w", p, err)
		}
	}
	for _, d := range []string{s.OutputDir(), s.LogsDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	return nil
}

// PruneIntermediate は、ギャラリーに保存するスキャンから、もう使わない中間ファイルを消す。
//
// 消すのは dense/（歪みを補正した写真の複製や、メッシュを作る途中のファイル）だけ。容量の大半を占め、
// 成果物はすべて output/ にあるので、表示にもダウンロードにも要らない。
// 残すもの: images/（元の写真）、database.db と sparse/（特徴点とカメラ位置。VPS などで地図として使える）、
// output/、logs/、thumbnail.jpg。
func (s *Scan) PruneIntermediate() error {
	if err := os.RemoveAll(s.DenseDir()); err != nil {
		return fmt.Errorf("%s を削除できません: %w", s.DenseDir(), err)
	}
	return nil
}

// List は scans/ にある作業ディレクトリの ID を返す（UUID 形式の名前だけ）。
// DB に行が無いのに残っているディレクトリ（孤児）を見つけるために使う。
func (r *Root) List() ([]string, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && scan.ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
