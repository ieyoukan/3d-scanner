package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

func newTestScan(t *testing.T) (*Root, *Scan, string) {
	t.Helper()
	root, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := scan.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := root.Scan(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Create(); err != nil {
		t.Fatal(err)
	}
	return root, ws, id
}

// TestScanRejectsBadID: ID の形式検査（1つ目の関門）。
func TestScanRejectsBadID(t *testing.T) {
	root, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "..", "../x", "a/b", "00000000-0000-4000-8000-00000000000Z"} {
		if _, err := root.Scan(id); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("Scan(%q) err = %v, want ErrOutsideRoot", id, err)
		}
	}
}

// TestResolve: 相対パスの検査（2つ目の関門）。
func TestResolve(t *testing.T) {
	_, ws, _ := newTestScan(t)

	ok := []string{"output/mesh.glb", "./output/mesh.glb", "output/../output/mesh.glb", "logs/sfm.log"}
	for _, rel := range ok {
		got, err := ws.Resolve(rel)
		if err != nil {
			t.Errorf("Resolve(%q) が失敗: %v", rel, err)
			continue
		}
		if !within(ws.Dir(), got) {
			t.Errorf("Resolve(%q) = %q が作業ディレクトリの外", rel, got)
		}
	}

	bad := []string{"", "/etc/passwd", "../other/output/mesh.glb", "output/../../x", "../../../../etc/passwd"}
	for _, rel := range bad {
		if _, err := ws.Resolve(rel); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("Resolve(%q) err = %v, want ErrOutsideRoot", rel, err)
		}
	}
}

// TestResolveRejectsSymlinkEscape: 作業ディレクトリ内のシンボリックリンクが外を指していたら拒否する。
// 外部CLIの出力や壊れた状態から、サーバー上の無関係なファイルを配信しないための検査。
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	_, ws, _ := newTestScan(t)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("秘密"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws.OutputDir(), "mesh.glb")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("シンボリックリンクを作れない環境: %v", err)
	}
	if _, err := ws.Resolve("output/mesh.glb"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("外を指すリンクが通ってしまった: err = %v", err)
	}
}

// TestWithinDoesNotMatchSiblingPrefix: 名前が前方一致するだけの隣のディレクトリを「中」と誤判定しない。
func TestWithinDoesNotMatchSiblingPrefix(t *testing.T) {
	if within("/data/scans/abc", "/data/scans/abcdef/x") {
		t.Fatal("前方一致だけで配下と判定してしまった")
	}
	if !within("/data/scans/abc", "/data/scans/abc/x") || !within("/data/scans/abc", "/data/scans/abc") {
		t.Fatal("配下・自身を配下と判定できない")
	}
}

func TestRelRoundTrip(t *testing.T) {
	_, ws, _ := newTestScan(t)
	abs := filepath.Join(ws.OutputDir(), "mesh.glb")
	rel, err := ws.Rel(abs)
	if err != nil || rel != "output/mesh.glb" {
		t.Fatalf("Rel = %q, %v", rel, err)
	}
	if _, err := ws.Rel("/etc/passwd"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("外のパスの Rel: %v", err)
	}
}

// TestResetDerivedKeepsImages: やり直しの前の掃除は、写真を残して途中経過だけ消す。
func TestResetDerivedKeepsImages(t *testing.T) {
	_, ws, _ := newTestScan(t)
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	photo := filepath.Join(ws.ImagesDir(), "frame_0001.jpg")
	write(photo)
	write(ws.DatabasePath())
	write(filepath.Join(ws.SparseDir(), "0", "cameras.bin"))
	write(filepath.Join(ws.OutputDir(), "mesh.glb"))
	write(ws.LogPath(scan.StepSfM))

	if err := ws.ResetDerived(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(photo); err != nil {
		t.Fatalf("写真が消えた: %v", err)
	}
	for _, gone := range []string{ws.DatabasePath(), ws.SparseDir(), filepath.Join(ws.OutputDir(), "mesh.glb"), ws.LogPath(scan.StepSfM)} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s が残っている（err=%v）", gone, err)
		}
	}
	// output/ と logs/ は空で作り直されている（次の実行がそのまま書ける）。
	for _, d := range []string{ws.OutputDir(), ws.LogsDir()} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s が作り直されていない", d)
		}
	}
}

func TestRemoveAndList(t *testing.T) {
	root, ws, id := newTestScan(t)
	// UUID 形式でない名前のディレクトリは一覧に出さない（自分たちが作ったもの以外に触らない）。
	if err := os.Mkdir(filepath.Join(root.Dir(), "lost+found"), 0o750); err != nil {
		t.Fatal(err)
	}
	ids, err := root.List()
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("List = %v, %v", ids, err)
	}
	if err := ws.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := ws.Remove(); err != nil {
		t.Fatalf("2回目の Remove は何もせず成功するはず: %v", err)
	}
	if ids, _ := root.List(); len(ids) != 0 {
		t.Fatalf("削除後の List = %v", ids)
	}
}

func TestFreeBytes(t *testing.T) {
	root, _, _ := newTestScan(t)
	free, err := root.FreeBytes()
	if err != nil || free <= 0 {
		t.Fatalf("FreeBytes = %d, %v", free, err)
	}
}

// writeMap は SfM が成功したスキャンと同じ形の地図（DB と sparse/0）と写真を置く。
func writeMap(t *testing.T, ws *Scan, images ...string) {
	t.Helper()
	for _, name := range images {
		if err := os.WriteFile(filepath.Join(ws.ImagesDir(), name), []byte("jpeg:"+name), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(ws.ModelDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		ws.DatabasePath():                               "sqlite",
		filepath.Join(ws.ModelDir(), "images.bin"):      "images",
		filepath.Join(ws.ModelDir(), "cameras.bin"):     "cameras",
		filepath.Join(ws.ModelDir(), "points3D.bin"):    "points",
		ws.ThumbnailPath():                              "thumb",
		filepath.Join(ws.OutputDir(), "mesh.glb"):       "glTF",
		filepath.Join(ws.SparseDir(), "1", "other.bin"): "smaller model",
	} {
		os.MkdirAll(filepath.Dir(path), 0o750)
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInheritFrom: 「続きを撮る」で元の版の写真と地図を引き継ぐ。
func TestInheritFrom(t *testing.T) {
	root, parent, _ := newTestScan(t)
	writeMap(t, parent, "frame_0001.jpg", "frame_0002.png", "frame_0010.jpg")
	if !parent.HasMap() || parent.HasBase() {
		t.Fatalf("元の版: HasMap=%v HasBase=%v", parent.HasMap(), parent.HasBase())
	}

	childID, _ := scan.NewID()
	child, _ := root.Scan(childID)
	if err := child.Create(); err != nil {
		t.Fatal(err)
	}
	got, err := child.InheritFrom(parent)
	if err != nil {
		t.Fatal(err)
	}
	// 番号の最大は 10 なので、追加する写真は 11 番から（欠番があっても重ならない）。
	if got.Frames != 3 || got.NextNumber != 11 {
		t.Fatalf("InheritFrom = %+v", got)
	}
	if !child.HasBase() || child.HasMap() {
		t.Fatalf("続きの版: HasBase=%v HasMap=%v（自分の地図はまだ無い）", child.HasBase(), child.HasMap())
	}
	names, err := child.BaseFrames()
	if err != nil || len(names) != 3 || !names["frame_0002.png"] {
		t.Fatalf("BaseFrames = %v, %v", names, err)
	}

	// ハードリンクなので、元の版を丸ごと消しても引き継いだファイルは読める。
	if err := parent.Remove(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(child.ImagesDir(), "frame_0010.jpg"): "jpeg:frame_0010.jpg",
		child.BaseDatabasePath():                           "sqlite",
		filepath.Join(child.BaseModelDir(), "images.bin"):  "images",
		child.ThumbnailPath():                              "thumb",
	} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", path, b, err)
		}
	}
	// 引き継ぐのは sparse/0 だけ。成果物や他のモデルは持ってこない。
	for _, p := range []string{filepath.Join(child.OutputDir(), "mesh.glb"), filepath.Join(child.BaseModelDir(), "other.bin")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s は引き継がないはず", p)
		}
	}

	// 完成して保存すると、引き継いだ地図（base/）は要らなくなるので消す。写真は残す。
	if err := child.PruneIntermediate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child.BaseDir()); !os.IsNotExist(err) {
		t.Fatalf("base/ が残っている: %v", err)
	}
	if _, err := os.Stat(filepath.Join(child.ImagesDir(), "frame_0001.jpg")); err != nil {
		t.Fatalf("写真が消えた: %v", err)
	}
}

// TestInheritFromRequiresMap: 地図の無いスキャン（SfM が失敗した等）からは続きを撮れない。
func TestInheritFromRequiresMap(t *testing.T) {
	root, parent, _ := newTestScan(t)
	childID, _ := scan.NewID()
	child, _ := root.Scan(childID)
	child.Create()
	if _, err := child.InheritFrom(parent); err == nil {
		t.Fatal("地図が無いのに引き継げた")
	}
}

// TestTempDir: 一時作業の置き場は作れて、起動時の掃除で空になる。
func TestTempDir(t *testing.T) {
	root, _, _ := newTestScan(t)
	dir, err := root.NewTemp("vps-")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "database.db"), []byte("x"), 0o640)
	if err := root.CleanTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("一時ディレクトリが残っている: %v", err)
	}
}
