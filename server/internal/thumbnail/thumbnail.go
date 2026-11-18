// Package thumbnail はギャラリー用の縮小画像を作る。
//
// 外部のライブラリ（画像処理の拡張パッケージや ImageMagick）は使わず、Go 標準の image パッケージだけで
// 「長辺を maxSide に縮め、JPEG で保存する」ことをする。縮小は面積平均法（出力の1画素に対応する
// 入力の画素をすべて平均する）。単純な間引きより、縮小したときのちらつき（モアレ）が出にくい。
package thumbnail

import (
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // PNG の写真も読めるように、デコーダを登録する。
	"os"
	"path/filepath"
)

// Make は src の画像を長辺 maxSide 以下に縮め、dst に JPEG で書く。
//
// 書き込みは「一時ファイルに書いてから名前を変える」形にする。途中で失敗しても、
// 壊れた（途中までの）サムネイルが dst に残らない。
// 失敗条件: src が読めない・画像でない、dst のディレクトリに書けない。
func Make(src, dst string, maxSide int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("画像を読めません: %w", err)
	}
	small := Resize(img, maxSide)

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".thumb-*.jpg")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // 名前を変えた後なら何もしない（既に無い）。
	if err := jpeg.Encode(tmp, small, &jpeg.Options{Quality: 82}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Resize は長辺が maxSide 以下になるよう縮めた画像を返す。元から小さければ縮めない（拡大はしない）。
func Resize(src image.Image, maxSide int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if w >= h && w > maxSide {
		nw, nh = maxSide, max(1, h*maxSide/w)
	} else if h > w && h > maxSide {
		nw, nh = max(1, w*maxSide/h), maxSide
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		// 出力の (x, y) に対応する入力の範囲 [x0, x1) × [y0, y1)。
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		if y1 == y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			if x1 == x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					// RGBA() は 0〜65535 の値を返す（色の種類によらず同じ範囲にそろう）。
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / n >> 8)
			dst.Pix[i+1] = uint8(g / n >> 8)
			dst.Pix[i+2] = uint8(bl / n >> 8)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return dst
}
