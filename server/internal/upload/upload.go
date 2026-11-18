// Package upload はアップロードされた写真を検証しながら PVC に保存する。
//
// 方針: 「信用できない入力を、無制限には受け取らない」。
// 利用者が送ってくるのは写真のはずだが、サーバーから見ればただのバイト列であり、
// 巨大なファイル、画像のふりをした別物、数千枚の送信なども届きうる。
// ここでは上限と形式を1枚ずつ確かめ、1つでも問題があれば全体を断る。
package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	// image.DecodeConfig が JPEG と PNG のヘッダを読めるよう、デコーダを登録する。
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// Error は利用者に原因を伝えられる検証エラー。
// Code は画面側が分岐に使う短い識別子、Message はそのまま表示できる日本語。
type Error struct {
	Code    string
	Message string
	// Status は対応する HTTP ステータス（400 / 413 / 415 など）。
	Status int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func reject(status int, code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Status: status}
}

// Result は保存できた写真と付加情報。
type Result struct {
	Frames []scan.Frame
	// DuplicatesSkipped は中身が完全に同じだったため保存しなかった枚数。
	DuplicatesSkipped int
	// UniformSize は全ての写真の幅と高さが同じかどうか。
	// 同じなら「1台のカメラ・同じ設定で撮った」と仮定でき、SfM でカメラの内部パラメータを
	// 全画像で共有できる（推定する未知数が減って安定する）。
	UniformSize bool
	// ClientMeta は撮影側が送った端末情報などの JSON（無ければ nil）。
	ClientMeta json.RawMessage
}

// 画像以外のフォーム項目の上限。メタデータは小さな JSON なので 64KB あれば十分。
const maxMetadataBytes = 64 * 1024

// 先頭の何バイトでファイルの種類を判定するか（http.DetectContentType が見る長さ）。
const sniffLen = 512

// Receive は multipart/form-data を先頭から順に読み、写真を imagesDir に保存する。
//
// 入力:
//   - mr: リクエストボディの multipart リーダー。
//     項目名 "images"（または "images[]"、"image"）が写真、"metadata" が任意の JSON。
//   - imagesDir: 保存先（作成済みであること）。
//   - lim: 枚数・サイズ・画素数の上限。
//
// 出力: 保存した写真の一覧（ファイル名はサーバーが frame_0001.jpg のように付ける）。
// 副作用: imagesDir にファイルを書く。エラーを返した場合も書きかけが残りうるので、
// 呼び出し側が作業ディレクトリごと削除すること。
// 失敗条件: 上限超過・未対応形式・壊れた画像・枚数不足（*Error）、または I/O エラー。
//
// なぜ r.ParseMultipartForm を使わないのか:
// あちらは全項目を読み終えてから返すので、上限を超える入力でもまず一時ファイルに書いてしまう。
// 1項目ずつ読めば、問題のある項目に当たった時点で残りを読まずに断れる。
func Receive(mr *multipart.Reader, imagesDir string, lim config.Limits) (Result, error) {
	return ReceiveWith(mr, imagesDir, lim, Options{})
}

// Options は Receive の既定を変えるときに使う。ゼロ値の項目は既定のまま。
type Options struct {
	// FirstNumber は最初の写真に付ける番号（frame_<番号>.jpg）。既定は 1。
	// 「続きを撮る」では、引き継いだ写真と名前が重ならないよう続きの番号から始める。
	FirstNumber int
	// MinImages / MaxImages は今回受け取る枚数の範囲。既定は lim の値。
	MinImages int
	MaxImages int
}

// ReceiveWith は Options を指定できる Receive。
// 利用者に見せる「◯枚目」は、番号ではなく今回送られてきた順番で数える。
func ReceiveWith(mr *multipart.Reader, imagesDir string, lim config.Limits, opts Options) (Result, error) {
	if opts.FirstNumber <= 0 {
		opts.FirstNumber = 1
	}
	if opts.MinImages <= 0 {
		opts.MinImages = lim.MinImages
	}
	if opts.MaxImages <= 0 {
		opts.MaxImages = lim.MaxImages
	}
	var res Result
	var metadata []byte
	// seen は「内容のハッシュ → 既に保存済み」。完全に同じ写真の二重送信を省くのに使う。
	seen := map[string]bool{}
	// partIndex は送られてきた写真の通し番号（重複で捨てた分も数える）。
	// メタデータの frames 配列はこの番号で写真と対応づける。
	partIndex := 0
	// origIndex[i] は「保存した i 番目の写真」が、送信時の何番目だったか。
	var origIndex []int

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, bodyError(err)
		}

		switch part.FormName() {
		case "images", "images[]", "image": // "image" は写真1枚だけを送る位置合わせ用
			if len(res.Frames) >= opts.MaxImages {
				return res, reject(http.StatusRequestEntityTooLarge, "too_many_images",
					"写真は最大%d枚までです。枚数を減らしてください。", opts.MaxImages)
			}
			seq := len(res.Frames) + 1
			frame, dup, err := saveImage(part, imagesDir, seq, opts.FirstNumber+seq-1, lim, seen)
			part.Close()
			if err != nil {
				return res, err
			}
			if dup {
				res.DuplicatesSkipped++
			} else {
				res.Frames = append(res.Frames, frame)
				origIndex = append(origIndex, partIndex)
			}
			partIndex++

		case "metadata":
			// 上限+1 バイトまで読む。上限ちょうどで切ると「収まった」のか「溢れた」のか区別できない。
			b, err := io.ReadAll(io.LimitReader(part, maxMetadataBytes+1))
			part.Close()
			if err != nil {
				return res, bodyError(err)
			}
			if len(b) > maxMetadataBytes {
				return res, reject(http.StatusRequestEntityTooLarge, "metadata_too_large", "メタデータが大きすぎます。")
			}
			metadata = b

		default:
			// 知らない項目は読まずに捨てる（NextPart が残りを読み飛ばす）。
			part.Close()
		}
	}

	if len(res.Frames) < opts.MinImages {
		msg := fmt.Sprintf("写真が足りません（%d枚）。少なくとも%d枚、できれば20枚以上を、少しずつ位置を変えて撮ってください。",
			len(res.Frames), opts.MinImages)
		if opts.MinImages == 1 {
			msg = "写真がありません。少なくとも1枚送ってください。"
		}
		if res.DuplicatesSkipped > 0 {
			msg += fmt.Sprintf("（同じ内容の写真%d枚は除きました）", res.DuplicatesSkipped)
		}
		return res, reject(http.StatusBadRequest, "too_few_images", "%s", msg)
	}

	res.UniformSize = uniformSize(res.Frames)
	applyMetadata(&res, metadata, origIndex)
	return res, nil
}

// saveImage は写真1枚を検証して保存する。
//
// 入力: number は利用者に見せる「◯枚目」、fileNumber はファイル名に付ける番号。
// 出力: 保存した写真の記録。dup が true のときは既にある写真と同じ内容なので保存していない。
// 失敗条件: 大きすぎる・未対応形式・壊れている・画素数や辺の長さが範囲外。
func saveImage(part io.Reader, dir string, number, fileNumber int, lim config.Limits, seen map[string]bool) (frame scan.Frame, dup bool, err error) {
	// まず一時的な名前で書く。検証を全部通ってから正式な名前に変える。
	tmpPath := filepath.Join(dir, fmt.Sprintf("frame_%04d.part", fileNumber))
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return frame, false, fmt.Errorf("写真の保存先を開けません: %w", err)
	}
	// どの経路で抜けても一時ファイルを残さない（成功時は Rename 済みなので Remove は空振りする）。
	defer os.Remove(tmpPath)
	defer f.Close()

	// 1回の読み取りで「ディスクへの保存」「ハッシュ計算」「先頭部分の確保」を同時に行う。
	//   LimitReader: 上限+1 バイトで打ち切る（上限を超えたかどうかを後で判定するため +1）。
	//   MultiWriter: 読んだバイトをファイルとハッシュ計算の両方に流す。
	hash := sha256.New()
	head := &headBuffer{max: sniffLen}
	size, err := io.Copy(io.MultiWriter(f, hash, head), io.LimitReader(part, lim.MaxImageBytes+1))
	if err != nil {
		return frame, false, bodyError(err)
	}
	if size > lim.MaxImageBytes {
		return frame, false, reject(http.StatusRequestEntityTooLarge, "image_too_large",
			"%d枚目の写真が大きすぎます（上限 %dMB）。", number, lim.MaxImageBytes/(1024*1024))
	}
	if size == 0 {
		return frame, false, reject(http.StatusBadRequest, "empty_image", "%d枚目の写真が空です。", number)
	}

	// 種類の判定は「中身の先頭バイト（マジックナンバー）」で行う。
	// ファイル名の拡張子やブラウザが申告する Content-Type は利用者が自由に偽れるので使わない。
	var ext string
	switch ct := http.DetectContentType(head.buf); ct {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	default:
		return frame, false, reject(http.StatusUnsupportedMediaType, "unsupported_type",
			"%d枚目は対応していない形式です（%s）。JPEG か PNG の写真を送ってください。", number, ct)
	}

	sum := hex.EncodeToString(hash.Sum(nil))
	if seen[sum] {
		return frame, true, nil
	}

	if err := f.Close(); err != nil {
		return frame, false, fmt.Errorf("写真を書き込めません: %w", err)
	}

	// 幅と高さはヘッダだけを読んで調べる（DecodeConfig は画素を展開しない）。
	// 先に寸法を確かめることで、「数KBのファイルなのに展開すると数GB」という画像爆弾を
	// 後段の COLMAP に渡さずに済む。
	// 画素データ全体の破損まではここで調べない（全画像を展開すると数十秒かかるため）。
	// 壊れた画像は COLMAP の特徴抽出が読めずに飛ばし、「位置を推定できた枚数」の減少として現れる。
	width, height, err := dimensions(tmpPath)
	if err != nil {
		return frame, false, reject(http.StatusBadRequest, "broken_image",
			"%d枚目の写真を読み込めません。ファイルが壊れている可能性があります。", number)
	}
	if int64(width)*int64(height) > lim.MaxImagePixels {
		return frame, false, reject(http.StatusRequestEntityTooLarge, "image_too_many_pixels",
			"%d枚目の写真の解像度が高すぎます（%d×%d）。上限は約%dメガピクセルです。",
			number, width, height, lim.MaxImagePixels/1_000_000)
	}
	if min(width, height) < lim.MinImageSide {
		return frame, false, reject(http.StatusBadRequest, "image_too_small",
			"%d枚目の写真が小さすぎます（%d×%d）。短辺%dピクセル以上の写真を使ってください。",
			number, width, height, lim.MinImageSide)
	}

	name := fmt.Sprintf("frame_%04d%s", fileNumber, ext)
	if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
		return frame, false, fmt.Errorf("写真を保存できません: %w", err)
	}
	seen[sum] = true
	return scan.Frame{
		Index:    fileNumber - 1,
		FileName: name,
		Width:    width,
		Height:   height,
		Bytes:    size,
		SHA256:   sum,
	}, false, nil
}

// dimensions は画像ファイルの幅と高さを返す。
func dimensions(path string) (width, height int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, errors.New("幅または高さが0です")
	}
	return cfg.Width, cfg.Height, nil
}

// uniformSize は全フレームの寸法が同じかを返す。
func uniformSize(frames []scan.Frame) bool {
	for _, f := range frames[1:] {
		if f.Width != frames[0].Width || f.Height != frames[0].Height {
			return false
		}
	}
	return true
}

// clientMetadata はクライアントが送る任意のメタデータの形。
// 知らない項目があっても無視する（将来クライアントが項目を増やしてもサーバーが壊れない）。
type clientMetadata struct {
	// Client は端末・ブラウザなどの情報。中身は解釈せず JSON のまま保存する。
	Client json.RawMessage `json:"client"`
	// Frames は写真ごとの情報。送信した写真と同じ順番で並ぶ。
	Frames []json.RawMessage `json:"frames"`
}

// applyMetadata はメタデータを写真の記録に反映する。
//
// メタデータは「あれば役立つ」付加情報であり、復元には必須ではない。
// だから形式がおかしくても全体を失敗にはせず、読めた分だけ使う。
func applyMetadata(res *Result, raw []byte, origIndex []int) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return
	}
	var meta clientMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return
	}
	if json.Valid(meta.Client) {
		res.ClientMeta = meta.Client
	}
	for i := range res.Frames {
		orig := origIndex[i]
		if orig >= len(meta.Frames) {
			continue
		}
		frameMeta := meta.Frames[orig]
		res.Frames[i].Camera = frameMeta
		// capturedAt（撮影時刻）だけは列として持つので取り出す。
		var t struct {
			CapturedAt string `json:"capturedAt"`
		}
		if json.Unmarshal(frameMeta, &t) == nil && t.CapturedAt != "" {
			if at, err := time.Parse(time.RFC3339, t.CapturedAt); err == nil {
				at = at.UTC()
				res.Frames[i].CapturedAt = &at
			}
		}
	}
}

// bodyError はリクエストボディの読み取りエラーを、利用者向けの検証エラーに直す。
func bodyError(err error) error {
	// http.MaxBytesReader（リクエスト全体の上限）に当たると *http.MaxBytesError が返る。
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return reject(http.StatusRequestEntityTooLarge, "upload_too_large",
			"アップロード全体が大きすぎます（上限 %dMB）。写真の枚数か解像度を減らしてください。", tooLarge.Limit/(1024*1024))
	}
	// それ以外は、通信が途中で切れた・multipart の形が壊れているなど。
	return reject(http.StatusBadRequest, "bad_upload", "アップロードを最後まで受け取れませんでした。通信状況を確認して、もう一度お試しください。")
}

// headBuffer は書き込まれた内容の先頭 max バイトだけを覚えておく Writer。
type headBuffer struct {
	max int
	buf []byte
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// FocalHint は、写真ごとのメタデータ（撮影側が EXIF から読んだ値）にある 35mm 換算の焦点距離から、
// 今回の写真全体の代表値を求める。
//
// 手掛かりとして使うのは、次をすべて満たすときだけ:
//   - 写真がすべて同じ寸法（1台のカメラ・同じ設定とみなせる）
//   - 半分以上の写真に焦点距離がある
//   - それらが中央値から ±5% に収まる（ズームを変えながら撮った写真には使わない）
//
// 外れた値を初期値に渡すと、渡さないより悪くなりうるので、迷ったら使わない。
func FocalHint(frames []scan.Frame) (focal35 float64, ok bool) {
	if len(frames) == 0 || !uniformSize(frames) {
		return 0, false
	}
	var values []float64
	for _, f := range frames {
		var c struct {
			FocalLength35mm float64 `json:"focalLength35mm"`
		}
		if len(f.Camera) > 0 && json.Unmarshal(f.Camera, &c) == nil && c.FocalLength35mm > 0 {
			values = append(values, c.FocalLength35mm)
		}
	}
	if len(values)*2 < len(frames) {
		return 0, false
	}
	sort.Float64s(values)
	median := values[len(values)/2]
	for _, v := range values {
		if v < median*0.95 || v > median*1.05 {
			return 0, false
		}
	}
	return median, true
}
