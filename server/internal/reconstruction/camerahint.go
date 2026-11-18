package reconstruction

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"

	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// CameraHint は、撮影側から分かったカメラの手掛かり（受付時に作業ディレクトリへ書く）。
//
// なぜ要るのか: SfM はカメラの焦点距離も推定するが、初期値が大きく外れていると、
// 写真どうしの位置関係が解けずに一部しかつながらないことがある。
// COLMAP は写真の EXIF から焦点距離を読んで初期値にする（ログに "(Prior)" と出る）が、
// ブラウザで縮小し直した写真には EXIF が残らず、長辺の 1.2 倍という仮の値から始めることになる
// （COLMAP 4.2.1 で確認）。そこで、撮影側が縮小前に読んだ焦点距離をここで受け渡す。
type CameraHint struct {
	// FocalLength35mm は 35mm 判に換算した焦点距離（mm）。スマホの広角カメラは 24〜28mm 前後。
	FocalLength35mm float64 `json:"focalLength35mm"`
	// Width と Height は、この焦点距離が当てはまる写真の寸法（送った写真の寸法）。
	Width  int `json:"width"`
	Height int `json:"height"`
}

// fullFrameDiagonalMM は 35mm 判（36×24mm）の対角線の長さ。35mm 換算の焦点距離は対角の画角で定義される。
var fullFrameDiagonalMM = math.Hypot(36, 24)

// FocalPixels は焦点距離を画素単位にする。画角が同じになるよう、対角線の比で換算する:
//
//	f[px] = f35[mm] × 写真の対角線[px] ÷ 35mm 判の対角線[mm]
func (h CameraHint) FocalPixels() float64 {
	return h.FocalLength35mm * math.Hypot(float64(h.Width), float64(h.Height)) / fullFrameDiagonalMM
}

// Valid は手掛かりとして使える値かどうか（スマホ・デジカメとしてありうる範囲）。
func (h CameraHint) Valid() bool {
	return h.FocalLength35mm >= 8 && h.FocalLength35mm <= 600 && h.Width > 0 && h.Height > 0
}

// cameraHintPath は作業ディレクトリの中の置き場所。
func cameraHintPath(ws *workspace.Scan) string { return filepath.Join(ws.Dir(), "camera_hint.json") }

// WriteCameraHint は手掛かりを作業ディレクトリに書く。
func WriteCameraHint(ws *workspace.Scan, h CameraHint) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return os.WriteFile(cameraHintPath(ws), b, 0o640)
}

// ReadCameraHint は手掛かりを読む。無い・読めない・値がおかしいときは ok=false。
func ReadCameraHint(ws *workspace.Scan) (CameraHint, bool) {
	b, err := os.ReadFile(cameraHintPath(ws))
	if errors.Is(err, os.ErrNotExist) || err != nil {
		return CameraHint{}, false
	}
	var h CameraHint
	if json.Unmarshal(b, &h) != nil || !h.Valid() {
		return CameraHint{}, false
	}
	return h, true
}
