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
//	  thumbnail.jpg  ギャラリー用の縮小画像
//	  base/          「続きを撮る」で作ったスキャンだけにある。元の版から引き継いだ地図
//	    database.db    元の版の COLMAP DB（特徴点とマッチ）
//	    model/         元の版の SfM モデル（sparse/0 の中身）
//	    frames.txt     元の版から引き継いだ写真のファイル名（1行1枚）
//
// images/ と sparse/0/ を同じ親の下に置くのは、OpenSplat が「プロジェクトのルート」を1つ受け取り、
// その直下の images/ と sparse/0/ を読むため。
package workspace

import (
	"errors"
	"fmt"
	"io"
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
	// tmp は一時作業の置き場（<DATA_DIR>/tmp）。VPS の位置合わせなど、結果を残さない処理に使う。
	// コンテナの /tmp ではなく PVC 上に置くのは、COLMAP の DB を複製するので容量が要るため。
	tmp string
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
	r := &Root{dir: filepath.Join(abs, "scans"), tmp: filepath.Join(abs, "tmp")}
	for _, d := range []string{r.dir, r.tmp} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("作業ディレクトリ %s を作れません: %w", d, err)
		}
	}
	return r, nil
}

// NewTemp は一時作業用の空のディレクトリを作る。使い終わったら呼び出し側が消す。
func (r *Root) NewTemp(prefix string) (string, error) {
	return os.MkdirTemp(r.tmp, prefix)
}

// CleanTemp は一時作業の置き場を空にする。起動時に呼び、前回のプロセスが
// 処理の途中で止まって消し損ねたものを片付ける（この時点では誰も使っていない）。
func (r *Root) CleanTemp() error {
	entries, err := os.ReadDir(r.tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(r.tmp, e.Name())); err != nil {
			return err
		}
	}
	return nil
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

// BaseDir は「続きを撮る」で元の版から引き継いだ地図の置き場を返す。
func (s *Scan) BaseDir() string          { return filepath.Join(s.dir, "base") }
func (s *Scan) BaseDatabasePath() string { return filepath.Join(s.BaseDir(), "database.db") }
func (s *Scan) BaseModelDir() string     { return filepath.Join(s.BaseDir(), "model") }

// BaseFramesPath は元の版から引き継いだ写真のファイル名の一覧の場所を返す。
func (s *Scan) BaseFramesPath() string { return filepath.Join(s.BaseDir(), "frames.txt") }

// HasBase は、このスキャンが「続きを撮る」で作ったもの（引き継いだ地図がある）かを返す。
func (s *Scan) HasBase() bool {
	return isFile(s.BaseDatabasePath()) && isFile(filepath.Join(s.BaseModelDir(), "images.bin"))
}

// HasMap は、位置合わせ（VPS）と「続きを撮る」に使える地図（COLMAP の DB と SfM モデル）が
// 揃っているかを返す。SfM が成功したスキャンにはあり、ギャラリーに保存しても消さない。
func (s *Scan) HasMap() bool {
	return isFile(s.DatabasePath()) && isFile(filepath.Join(s.ModelDir(), "images.bin"))
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

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
// 消すもの:
//   - dense/（歪みを補正した写真の複製や、メッシュを作る途中のファイル）。容量の大半を占め、
//     成果物はすべて output/ にあるので、表示にもダウンロードにも要らない。
//   - base/（続きの版が元の版から引き継いだ地図）。完成した時点で、自分の database.db と sparse/ が
//     元の地図を含んでいるので要らない。
//
// 残すもの: images/（元の写真）、database.db と sparse/（特徴点とカメラ位置。VPS の地図として使う）、
// output/、logs/、thumbnail.jpg。
func (s *Scan) PruneIntermediate() error {
	for _, d := range []string{s.DenseDir(), s.BaseDir()} {
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("%s を削除できません: %w", d, err)
		}
	}
	return nil
}

// Inherited は InheritFrom の結果。
type Inherited struct {
	// Frames は引き継いだ写真の枚数。
	Frames int
	// NextNumber は、今回追加する写真に付ける最初の番号（frame_<番号>.jpg）。
	// 引き継いだ写真と名前が重ならないよう、最大の番号の次から始める。
	NextNumber int
}

// InheritFrom は「続きを撮る」の受付時に呼び、元の版（parent）の写真と地図をこのスキャンに引き継ぐ。
//
// 引き継ぐもの:
//
//	parent/images/*      → images/*         （写真。同じ名前のまま）
//	parent/database.db   → base/database.db （特徴点とマッチ）
//	parent/sparse/0/*    → base/model/*     （カメラの位置と3D点）
//	parent/thumbnail.jpg → thumbnail.jpg    （ギャラリーで同じ見た目にする）
//
// コピーではなくハードリンクを使う。どれも元の版では二度と書き換えないファイルなので、
// 同じ中身を共有しても困らず、容量も時間もほぼ使わない。ハードリンクなので、後で元の版を削除しても
// こちらのファイルは消えない（ファイルの実体は、最後のリンクが消えたときに消える）。
// COLMAP が書き換える database.db は、ワーカーが処理を始めるときに base/ から複製する。
//
// 失敗条件: 元の版に地図（DB と SfM モデル）が無い、ファイルを作れない。
func (s *Scan) InheritFrom(parent *Scan) (Inherited, error) {
	var res Inherited
	if !parent.HasMap() {
		return res, errors.New("元のスキャンに地図（COLMAP の DB と SfM モデル）がありません")
	}
	if err := os.MkdirAll(s.BaseModelDir(), 0o750); err != nil {
		return res, err
	}

	entries, err := os.ReadDir(parent.ImagesDir())
	if err != nil {
		return res, err
	}
	var names []string
	maxNumber := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := linkOrCopy(filepath.Join(parent.ImagesDir(), e.Name()), filepath.Join(s.ImagesDir(), e.Name())); err != nil {
			return res, err
		}
		names = append(names, e.Name())
		var n int
		if _, err := fmt.Sscanf(e.Name(), "frame_%d.", &n); err == nil && n > maxNumber {
			maxNumber = n
		}
	}

	if err := linkOrCopy(parent.DatabasePath(), s.BaseDatabasePath()); err != nil {
		return res, err
	}
	model, err := os.ReadDir(parent.ModelDir())
	if err != nil {
		return res, err
	}
	for _, e := range model {
		if e.Type().IsRegular() {
			if err := linkOrCopy(filepath.Join(parent.ModelDir(), e.Name()), filepath.Join(s.BaseModelDir(), e.Name())); err != nil {
				return res, err
			}
		}
	}
	if isFile(parent.ThumbnailPath()) {
		if err := linkOrCopy(parent.ThumbnailPath(), s.ThumbnailPath()); err != nil {
			return res, err
		}
	}
	list := strings.Join(names, "\n")
	if list != "" {
		list += "\n"
	}
	if err := os.WriteFile(s.BaseFramesPath(), []byte(list), 0o640); err != nil {
		return res, err
	}
	return Inherited{Frames: len(names), NextNumber: maxNumber + 1}, nil
}

// BaseFrames は元の版から引き継いだ写真のファイル名を返す（続きの版でなければ空）。
func (s *Scan) BaseFrames() (map[string]bool, error) {
	b, err := os.ReadFile(s.BaseFramesPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names[line] = true
		}
	}
	return names, nil
}

// linkOrCopy は src を dst にハードリンクする。リンクを作れないファイルシステム（別の
// ディスクをまたぐ、対応していない）ではコピーする。
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return CopyFile(src, dst)
}

// CopyFile は src の中身を dst に複製する（dst があれば上書きする）。
// 書きかけのファイルを残さないよう、一時ファイルに書いてから名前を変える。
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
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
