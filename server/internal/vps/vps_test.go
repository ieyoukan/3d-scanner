package vps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/cmdrun"
	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// このテストは COLMAP を実行しない。偽の Runner が、本物が書くテキスト形式のモデルを置く。
// 本物の COLMAP 4.2.1 での確認結果は docs/ARCHITECTURE.md に記録している。

type fakeColmap struct {
	mu    sync.Mutex
	calls [][]string
	// registered が true なら、image_registrator が写真を地図に追加できたことにする。
	registered bool
	// block が nil でなければ、feature_extractor はそれが閉じられるまで待つ（同時実行のテスト用）。
	block chan struct{}
	// started は feature_extractor が始まったら閉じられる。
	started chan struct{}
	// pairs は matches_importer に渡された組の一覧ファイルの中身。
	pairs string
}

const mapImagesTxt = "# Image list\n" +
	"1 1 0 0 0 0 0 0 1 frame_0001.jpg\n10 20 5\n" +
	"2 1 0 0 0 -1 0 0 1 frame_0002.jpg\n\n" +
	"3 1 0 0 0 -2 0 0 1 frame_0003.jpg\n\n"

func (f *fakeColmap) Run(ctx context.Context, cmd cmdrun.Command) (cmdrun.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd.Args)
	f.mu.Unlock()
	arg := func(flag string) string {
		for i := 0; i+1 < len(cmd.Args); i++ {
			if cmd.Args[i] == flag {
				return cmd.Args[i+1]
			}
		}
		return ""
	}
	switch cmd.Args[0] {
	case "model_converter":
		out := arg("--output_path")
		images := mapImagesTxt
		cameras := "1 SIMPLE_RADIAL 64 48 50 32 24 0\n"
		if strings.Contains(arg("--input_path"), "vps-") && f.registered {
			// 位置合わせの結果: 地図の3枚に、写真が4枚目として加わっている。
			// 回転なし・並進 (0,0,-2) なので、カメラは (0,0,2) にあり +Z を向く。特徴点3つのうち2つが3D点に対応。
			images += "4 1 0 0 0 0 0 -2 1 vps_query.jpg\n1 2 7 3 4 -1 5 6 9\n"
		}
		os.WriteFile(filepath.Join(out, "images.txt"), []byte(images), 0o640)
		os.WriteFile(filepath.Join(out, "cameras.txt"), []byte(cameras), 0o640)
	case "feature_extractor":
		if f.started != nil {
			close(f.started)
		}
		if f.block != nil {
			select {
			case <-f.block:
			case <-ctx.Done():
				return cmdrun.Result{}, ctx.Err()
			}
		}
	case "matches_importer":
		b, _ := os.ReadFile(arg("--match_list_path"))
		f.pairs = string(b)
	}
	return cmdrun.Result{}, ctx.Err()
}

// newMap は地図（DB と sparse/0）を持つスキャンと、位置合わせする写真を用意する。
func newMap(t *testing.T) (*workspace.Root, *workspace.Scan, Query) {
	t.Helper()
	root, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := scan.NewID()
	ws, _ := root.Scan(id)
	ws.Create()
	os.MkdirAll(ws.ModelDir(), 0o750)
	os.WriteFile(ws.DatabasePath(), []byte("sqlite"), 0o640)
	os.WriteFile(filepath.Join(ws.ModelDir(), "images.bin"), []byte("bin"), 0o640)
	q := filepath.Join(t.TempDir(), "frame_0001.JPG")
	os.WriteFile(q, []byte("jpeg"), 0o640)
	return root, ws, Query{Path: q, Width: 64, Height: 48}
}

func newLocalizer(run cmdrun.Runner, root *workspace.Root) *Localizer {
	return New(config.Pipeline{ColmapBin: "colmap", ColmapMaxImageSize: 1600}, run, root, nil)
}

func TestLocalizeFound(t *testing.T) {
	root, ws, q := newMap(t)
	run := &fakeColmap{registered: true}
	res, err := newLocalizer(run, root).Localize(context.Background(), ws, q)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Localized || res.Position != [3]float64{0, 0, 2} || res.Forward != [3]float64{0, 0, 1} ||
		res.Matches != 2 || res.MapImages != 3 || res.ComparedImages != 3 {
		t.Fatalf("結果 = %+v", res)
	}

	var order []string
	for _, c := range run.calls {
		order = append(order, c[0])
	}
	want := "model_converter feature_extractor matches_importer image_registrator model_converter"
	if strings.Join(order, " ") != want {
		t.Fatalf("呼び出し順 = %v", order)
	}
	// 地図のカメラと寸法が同じなので、そのカメラを使い回す。
	if !strings.Contains(strings.Join(run.calls[1], " "), "--ImageReader.existing_camera_id 1") {
		t.Fatalf("feature_extractor の引数 = %v", run.calls[1])
	}
	// 写真の名前は地図の写真と重ならない名前に付け替える（元の名前が frame_0001 でも）。
	if !strings.HasPrefix(run.pairs, "vps_query.jpg frame_0001.jpg\n") || strings.Count(run.pairs, "\n") != 3 {
		t.Fatalf("組の一覧 = %q", run.pairs)
	}
	// 地図そのものは書き換えない。作業用の一時ディレクトリは残さない。
	if b, _ := os.ReadFile(ws.DatabasePath()); string(b) != "sqlite" {
		t.Fatalf("地図の DB が変わった: %q", b)
	}
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(root.Dir()), "tmp")); len(entries) != 0 {
		t.Fatalf("一時ディレクトリが残っている: %v", entries)
	}
}

func TestLocalizeNotFound(t *testing.T) {
	root, ws, q := newMap(t)
	q.Width = 4000 // 寸法が違うので、カメラは使い回さない
	run := &fakeColmap{registered: false}
	res, err := newLocalizer(run, root).Localize(context.Background(), ws, q)
	if err != nil {
		t.Fatal(err)
	}
	if res.Localized {
		t.Fatalf("位置が分かったことになっている: %+v", res)
	}
	if strings.Contains(strings.Join(run.calls[1], " "), "existing_camera_id") {
		t.Fatalf("寸法が違うのにカメラを使い回した: %v", run.calls[1])
	}
}

func TestLocalizeOneAtATime(t *testing.T) {
	root, ws, q := newMap(t)
	run := &fakeColmap{block: make(chan struct{}), started: make(chan struct{})}
	l := newLocalizer(run, root)
	done := make(chan error, 1)
	go func() {
		_, err := l.Localize(context.Background(), ws, q)
		done <- err
	}()
	<-run.started
	_, ws2, q2 := newMap(t)
	if _, err := l.Localize(context.Background(), ws2, q2); !errors.Is(err, ErrBusy) {
		t.Fatalf("2件目 = %v, want ErrBusy", err)
	}
	close(run.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLocalizeTimeout(t *testing.T) {
	root, ws, q := newMap(t)
	run := &fakeColmap{block: make(chan struct{})}
	l := newLocalizer(run, root)
	l.Timeout = 20 * time.Millisecond
	if _, err := l.Localize(context.Background(), ws, q); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestLocalizeRequiresMap(t *testing.T) {
	root, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := scan.NewID()
	ws, _ := root.Scan(id)
	ws.Create()
	if _, err := newLocalizer(&fakeColmap{}, root).Localize(context.Background(), ws, Query{}); err == nil {
		t.Fatal("地図が無いのに成功した")
	}
}

func TestSamplePairsSpreadsOverTheMap(t *testing.T) {
	var images []reconstruction.ImagePose
	for i := 100; i >= 1; i-- { // ID の逆順で渡しても、ID の順に並べ直して選ぶ
		images = append(images, reconstruction.ImagePose{ID: i, Name: fmt.Sprintf("frame_%04d.jpg", i)})
	}
	got := samplePairs(images, 4)
	var ids []int
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	if fmt.Sprint(ids) != "[1 26 51 76]" {
		t.Fatalf("選んだ ID = %v", ids)
	}
	if len(samplePairs(images[:3], 4)) != 3 {
		t.Fatal("上限より少なければ全部使う")
	}
}

func TestNearestPairsPrefersSameDirectionNearby(t *testing.T) {
	// x 軸に沿って 0〜9 に並んだカメラ。偶数は +Z、奇数は -Z を向く。ID は x+1。
	var images []reconstruction.ImagePose
	for x := 9; x >= 0; x-- {
		fwd := [3]float64{0, 0, 1}
		if x%2 == 1 {
			fwd = [3]float64{0, 0, -1}
		}
		images = append(images, reconstruction.ImagePose{ID: x + 1, Center: [3]float64{float64(x), 0, 0}, Forward: fwd})
	}
	ids := func(got []reconstruction.ImagePose) string {
		var out []int
		for _, g := range got {
			out = append(out, g.ID)
		}
		return fmt.Sprint(out)
	}
	// x=4.2 から +Z を向いている: すぐ隣でも逆を向いた写真（x=3, 5）より、同じ向きの x=2, 4, 6 を選ぶ。
	if got := ids(nearestPairs(images, Near{Position: [3]float64{4.2, 0, 0}, Forward: [3]float64{0, 0, 3}}, 3)); got != "[3 5 7]" {
		t.Fatalf("選んだ ID = %s", got)
	}
	// 向きが分からなければ、近い順。
	if got := ids(nearestPairs(images, Near{Position: [3]float64{4.2, 0, 0}}, 3)); got != "[4 5 6]" {
		t.Fatalf("向き無しで選んだ ID = %s", got)
	}
	if len(nearestPairs(images[:2], Near{}, 3)) != 2 {
		t.Fatal("上限より少なければ全部使う")
	}
}

func TestLocalizeNearComparesOnlyNearbyImages(t *testing.T) {
	root, ws, q := newMap(t)
	run := &fakeColmap{registered: true}
	l := newLocalizer(run, root)
	l.NearPairs = 1
	// 地図の 3 枚は (0,0,0)・(1,0,0)・(2,0,0) にあり、どれも +Z を向く。
	q.Near = &Near{Position: [3]float64{1.9, 0, 0}, Forward: [3]float64{0, 0, 1}}
	res, err := l.Localize(context.Background(), ws, q)
	if err != nil {
		t.Fatal(err)
	}
	if run.pairs != "vps_query.jpg frame_0003.jpg\n" || res.ComparedImages != 1 || res.MapImages != 3 || !res.Localized {
		t.Fatalf("組の一覧 = %q / 結果 = %+v", run.pairs, res)
	}
}

func TestLocalizeReusesTheMapUntilItChanges(t *testing.T) {
	root, ws, q := newMap(t)
	run := &fakeColmap{registered: true}
	l := newLocalizer(run, root)
	mapReads := func() int {
		n := 0
		for _, c := range run.calls {
			if c[0] == "model_converter" && strings.Contains(strings.Join(c, " "), ws.ModelDir()) {
				n++
			}
		}
		return n
	}
	again := func() {
		t.Helper()
		os.WriteFile(q.Path, []byte("jpeg"), 0o640) // 写真は位置合わせのたびに移されるので置き直す
		if res, err := l.Localize(context.Background(), ws, q); err != nil || !res.Localized || res.MapImages != 3 {
			t.Fatalf("Localize = %+v, %v", res, err)
		}
	}
	again()
	again()
	if n := mapReads(); n != 1 {
		t.Fatalf("同じ地図を %d 回変換した", n)
	}
	// 地図が変わった（続きを撮って作り直した等）ら読み直す。
	os.WriteFile(filepath.Join(ws.ModelDir(), "images.bin"), []byte("new map"), 0o640)
	again()
	if n := mapReads(); n != 2 {
		t.Fatalf("地図が変わったのに読み直さなかった（変換 %d 回）", n)
	}
}
