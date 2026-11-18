package thumbnail

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func TestResizeKeepsAspectAndNeverEnlarges(t *testing.T) {
	cases := []struct{ w, h, max, ww, wh int }{
		{2048, 1536, 480, 480, 360}, // 横長
		{1536, 2048, 480, 360, 480}, // 縦長
		{300, 200, 480, 300, 200},   // 小さいものは拡大しない
		{5000, 1, 480, 480, 1},      // 極端に細長くても高さ 0 にならない
	}
	for _, c := range cases {
		got := Resize(image.NewRGBA(image.Rect(0, 0, c.w, c.h)), c.max).Bounds()
		if got.Dx() != c.ww || got.Dy() != c.wh {
			t.Errorf("%dx%d → %dx%d（期待 %dx%d）", c.w, c.h, got.Dx(), got.Dy(), c.ww, c.wh)
		}
	}
}

// TestResizeAverages: 縮小は面積平均。白黒の縞を半分に縮めると灰色になる（間引きなら白か黒のどちらか）。
func TestResizeAverages(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for x := 0; x < 4; x++ {
		for y := 0; y < 2; y++ {
			v := uint8(0)
			if x%2 == 0 {
				v = 255
			}
			src.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	got := Resize(src, 2).RGBAAt(0, 0)
	if got.R < 120 || got.R > 135 {
		t.Errorf("平均されていない: %v", got)
	}
}

func TestMakeWritesJPEG(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "frame_0001.jpg")
	f, _ := os.Create(src)
	jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 1200, 900)), nil)
	f.Close()

	dst := filepath.Join(dir, "thumbnail.jpg")
	if err := Make(src, dst, 480); err != nil {
		t.Fatal(err)
	}
	g, _ := os.Open(dst)
	defer g.Close()
	cfg, format, err := image.DecodeConfig(g)
	if err != nil || format != "jpeg" || cfg.Width != 480 || cfg.Height != 360 {
		t.Fatalf("出力 = %s %dx%d err=%v", format, cfg.Width, cfg.Height, err)
	}
	// 画像でないファイルは失敗し、出力を残さない。
	os.WriteFile(src, []byte("not an image"), 0o644)
	os.Remove(dst)
	if err := Make(src, dst, 480); err == nil {
		t.Fatal("画像でない入力で成功した")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("失敗したのに出力が残っている")
	}
}
