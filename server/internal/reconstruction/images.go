package reconstruction

import (
	"errors"
	"image"
	// DecodeConfig が JPEG / PNG のヘッダを読めるようにデコーダを登録する。
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
)

// inspectImages は images/ にある写真の枚数と、全て同じ寸法かどうかを返す。
//
// 寸法が揃っていれば「1台のカメラで同じ設定のまま撮った」とみなし、SfM でカメラの内部パラメータ
// （焦点距離など）を全画像で共有させる。受付時にも同じ判定をしているが、パイプラインを
// DB 無しで単体実行できるよう（cmd/reconstruct）、ここではファイルから直接調べ直す。
//
// 失敗条件: ディレクトリを読めない、または画像が1枚も無い。
func inspectImages(dir string) (count int, sameSize bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false, err
	}
	sameSize = true
	var w0, h0 int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return 0, false, err
		}
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			// 画像として読めないファイルは数えない（COLMAP も読み飛ばす）。
			continue
		}
		if count == 0 {
			w0, h0 = cfg.Width, cfg.Height
		} else if cfg.Width != w0 || cfg.Height != h0 {
			sameSize = false
		}
		count++
	}
	if count == 0 {
		return 0, false, errors.New("入力の写真がありません")
	}
	return count, sameSize, nil
}

// imagesMatchSize は、dir の写真のうち skip に無いもの（＝今回特徴を抽出する写真）が
// すべて w×h かどうかを返す。1枚も無ければ false。
func imagesMatchSize(dir string, skip map[string]bool, w, h int) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || skip[e.Name()] {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return false
		}
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			continue // 画像として読めないものは COLMAP も飛ばす
		}
		if cfg.Width != w || cfg.Height != h {
			return false
		}
		n++
	}
	return n > 0
}
