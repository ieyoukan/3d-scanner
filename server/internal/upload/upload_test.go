package upload

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/3d-scanner/server/internal/config"
)

// testLimits はテスト用の小さな上限。
func testLimits() config.Limits {
	return config.Limits{
		MinImages:      3,
		MaxImages:      5,
		MaxImageBytes:  200 * 1024,
		MaxUploadBytes: 1024 * 1024,
		MaxImagePixels: 1_000_000,
		MinImageSide:   32,
	}
}

// jpegBytes は seed ごとに中身が違う w×h の JPEG を作る。
func jpegBytes(t *testing.T, w, h int, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x) + seed, G: uint8(y) * seed, B: seed, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// filePart は multipart に入れるファイル1つ。
type filePart struct {
	field    string
	filename string
	data     []byte
}

// multipartReader は与えた項目から multipart/form-data を組み立て、そのリーダーを返す。
func multipartReader(t *testing.T, files []filePart, fields map[string]string) *multipart.Reader {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		fw, err := w.CreateFormFile(f.field, f.filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(f.data)
	}
	w.Close()
	return multipart.NewReader(&body, w.Boundary())
}

func images(t *testing.T, n int) []filePart {
	t.Helper()
	var files []filePart
	for i := 0; i < n; i++ {
		files = append(files, filePart{"images", "photo.jpg", jpegBytes(t, 64, 48, uint8(i+1))})
	}
	return files
}

// wantCode は err が指定のコードの *Error であることを確かめる。
func wantCode(t *testing.T, err error, code string, status int) {
	t.Helper()
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *Error(%s)", err, code)
	}
	if ue.Code != code || ue.Status != status {
		t.Fatalf("Error = {%s %d %q}, want {%s %d}", ue.Code, ue.Status, ue.Message, code, status)
	}
	if ue.Message == "" {
		t.Fatal("利用者向けの説明が空")
	}
}

func TestReceiveSavesWithServerChosenNames(t *testing.T) {
	dir := t.TempDir()
	files := []filePart{
		// 利用者のファイル名にパスを混ぜても、保存先の名前には一切使われない。
		{"images", "../../../etc/cron.d/evil.jpg", jpegBytes(t, 64, 48, 1)},
		{"images[]", "IMG_0002.JPG", jpegBytes(t, 64, 48, 2)}, // images[] という項目名も受け付ける
		{"images", "third.png", pngBytes(t, 64, 48)},
	}
	res, err := Receive(multipartReader(t, files, nil), dir, testLimits())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(res.Frames) != 3 || res.DuplicatesSkipped != 0 {
		t.Fatalf("frames=%d dup=%d", len(res.Frames), res.DuplicatesSkipped)
	}
	wantNames := []string{"frame_0001.jpg", "frame_0002.jpg", "frame_0003.png"}
	for i, f := range res.Frames {
		if f.FileName != wantNames[i] || f.Index != i || f.Width != 64 || f.Height != 48 || f.Bytes <= 0 || len(f.SHA256) != 64 {
			t.Errorf("frame %d = %+v", i, f)
		}
	}
	if !res.UniformSize {
		t.Error("全て同じ寸法なのに UniformSize が false")
	}

	// ディレクトリには決めた名前のファイルだけがある（.part の残骸や、外へ書かれたファイルが無い）。
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("保存先の中身 = %v", got)
	}
}

func TestReceiveSkipsExactDuplicates(t *testing.T) {
	same := jpegBytes(t, 64, 48, 7)
	files := []filePart{
		{"images", "a.jpg", same},
		{"images", "b.jpg", jpegBytes(t, 64, 48, 1)},
		{"images", "a-copy.jpg", same}, // 中身が完全に同じ
		{"images", "c.jpg", jpegBytes(t, 64, 48, 2)},
	}
	res, err := Receive(multipartReader(t, files, nil), t.TempDir(), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Frames) != 3 || res.DuplicatesSkipped != 1 {
		t.Fatalf("frames=%d dup=%d, want 3 と 1", len(res.Frames), res.DuplicatesSkipped)
	}
	// 重複を除いた後も番号は詰めて振られる。
	if res.Frames[2].FileName != "frame_0003.jpg" {
		t.Fatalf("3枚目の名前 = %s", res.Frames[2].FileName)
	}
}

func TestReceiveRejections(t *testing.T) {
	lim := testLimits()
	cases := []struct {
		name   string
		files  func(t *testing.T) []filePart
		code   string
		status int
	}{
		{"枚数不足", func(t *testing.T) []filePart { return images(t, 2) }, "too_few_images", http.StatusBadRequest},
		{"重複を除くと枚数不足", func(t *testing.T) []filePart {
			one := jpegBytes(t, 64, 48, 1)
			return []filePart{{"images", "a", one}, {"images", "b", one}, {"images", "c", one}}
		}, "too_few_images", http.StatusBadRequest},
		{"枚数超過", func(t *testing.T) []filePart { return images(t, 6) }, "too_many_images", http.StatusRequestEntityTooLarge},
		{"画像でない", func(t *testing.T) []filePart {
			return append(images(t, 2), filePart{"images", "note.jpg", []byte("これは写真ではなくテキストです")})
		}, "unsupported_type", http.StatusUnsupportedMediaType},
		{"拡張子だけ偽装したGIF", func(t *testing.T) []filePart {
			return append(images(t, 2), filePart{"images", "anim.jpg", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")})
		}, "unsupported_type", http.StatusUnsupportedMediaType},
		{"JPEGの先頭だけ本物で中身が壊れている", func(t *testing.T) []filePart {
			return append(images(t, 2), filePart{"images", "broken.jpg", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x00}, 100)...)})
		}, "broken_image", http.StatusBadRequest},
		{"1枚が大きすぎる", func(t *testing.T) []filePart {
			big := append(jpegBytes(t, 64, 48, 9), bytes.Repeat([]byte{0}, 201*1024)...)
			return append(images(t, 2), filePart{"images", "big.jpg", big})
		}, "image_too_large", http.StatusRequestEntityTooLarge},
		{"画素数が多すぎる", func(t *testing.T) []filePart {
			// 単色の PNG は 2000×2000 でも数KBにしかならない。ファイルサイズだけ見ていると通ってしまう例。
			return append(images(t, 2), filePart{"images", "huge.png", pngBytes(t, 2000, 2000)})
		}, "image_too_many_pixels", http.StatusRequestEntityTooLarge},
		{"小さすぎる", func(t *testing.T) []filePart {
			return append(images(t, 2), filePart{"images", "tiny.jpg", jpegBytes(t, 16, 16, 3)})
		}, "image_too_small", http.StatusBadRequest},
		{"空のファイル", func(t *testing.T) []filePart {
			return append(images(t, 2), filePart{"images", "empty.jpg", nil})
		}, "empty_image", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := Receive(multipartReader(t, c.files(t), nil), dir, lim)
			wantCode(t, err, c.code, c.status)
			// 断ったときに書きかけ（.part）を残さない。
			if left, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(left) != 0 {
				t.Errorf("一時ファイルが残っている: %v", left)
			}
		})
	}
}

// TestReceiveStopsAtRequestLimit: リクエスト全体の上限（http.MaxBytesReader）に当たったら 413 で断る。
func TestReceiveStopsAtRequestLimit(t *testing.T) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i < 4; i++ {
		fw, _ := w.CreateFormFile("images", "p.jpg")
		fw.Write(jpegBytes(t, 64, 48, uint8(i+1)))
	}
	w.Close()

	// ハンドラと同じ形でボディに上限を掛ける（ここでは2KBしか許さない）。
	rec := httptest.NewRecorder()
	limited := http.MaxBytesReader(rec, io.NopCloser(&body), 2*1024)
	_, err := Receive(multipart.NewReader(limited, w.Boundary()), t.TempDir(), testLimits())
	wantCode(t, err, "upload_too_large", http.StatusRequestEntityTooLarge)
}

func TestReceiveAppliesMetadataByOriginalOrder(t *testing.T) {
	same := jpegBytes(t, 64, 48, 5)
	files := []filePart{
		{"images", "0", same},
		{"images", "1", same}, // 重複 → 保存されない。メタデータの frames[1] は使われない
		{"images", "2", jpegBytes(t, 64, 48, 1)},
		{"images", "3", jpegBytes(t, 80, 48, 2)}, // 寸法が違う
	}
	meta := `{
		"client": {"userAgent": "test-agent", "facingMode": "environment"},
		"frames": [
			{"capturedAt": "2026-11-16T09:00:00+09:00", "source": "camera"},
			{"capturedAt": "2026-11-16T09:00:01+09:00", "source": "camera"},
			{"capturedAt": "2026-11-16T09:00:02+09:00", "source": "camera"},
			{"capturedAt": "時刻ではない", "source": "file"}
		]
	}`
	res, err := Receive(multipartReader(t, files, map[string]string{"metadata": meta, "unknown": "無視される"}), t.TempDir(), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Frames) != 3 || res.DuplicatesSkipped != 1 {
		t.Fatalf("frames=%d dup=%d", len(res.Frames), res.DuplicatesSkipped)
	}
	if !strings.Contains(string(res.ClientMeta), "test-agent") {
		t.Errorf("ClientMeta = %s", res.ClientMeta)
	}
	// 保存した2枚目は、送信時の3番目（frames[2]）のメタデータを受け取る。
	second := res.Frames[1]
	if second.CapturedAt == nil || second.CapturedAt.Format("15:04:05Z07:00") != "00:00:02Z" {
		t.Errorf("2枚目の撮影時刻 = %v（UTC の 00:00:02 のはず）", second.CapturedAt)
	}
	// 時刻として読めない値は捨てるが、他の項目は JSON のまま残る。
	third := res.Frames[2]
	if third.CapturedAt != nil || !strings.Contains(string(third.Camera), `"source": "file"`) {
		t.Errorf("3枚目 = %+v", third)
	}
	if res.UniformSize {
		t.Error("寸法の違う写真が混ざっているのに UniformSize が true")
	}
}

// TestReceiveIgnoresBrokenMetadata: メタデータは任意の付加情報。壊れていても写真は受け付ける。
func TestReceiveIgnoresBrokenMetadata(t *testing.T) {
	res, err := Receive(multipartReader(t, images(t, 3), map[string]string{"metadata": "{壊れたJSON"}), t.TempDir(), testLimits())
	if err != nil {
		t.Fatalf("メタデータの不備で全体が失敗した: %v", err)
	}
	if res.ClientMeta != nil || res.Frames[0].Camera != nil {
		t.Errorf("壊れたメタデータが使われた: %+v", res)
	}
}

func TestReceiveRejectsOversizedMetadata(t *testing.T) {
	huge := `{"client":"` + strings.Repeat("x", maxMetadataBytes) + `"}`
	_, err := Receive(multipartReader(t, images(t, 3), map[string]string{"metadata": huge}), t.TempDir(), testLimits())
	wantCode(t, err, "metadata_too_large", http.StatusRequestEntityTooLarge)
}
