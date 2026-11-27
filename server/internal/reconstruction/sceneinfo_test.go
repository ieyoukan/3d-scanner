package reconstruction

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func nearVec(a, b [3]float64) bool { return near(a[0], b[0]) && near(a[1], b[1]) && near(a[2], b[2]) }

// TestPoseFromQuaternion: 「世界→カメラ」の変換から、カメラの位置と下方向を正しく取り出せること。
// 式を手で確かめられる単純な姿勢を使う。
func TestPoseFromQuaternion(t *testing.T) {
	s := math.Sqrt(0.5)
	cases := []struct {
		name           string
		qw, qx, qy, qz float64
		t              [3]float64
		wantCenter     [3]float64
		wantDown       [3]float64
	}{
		{
			// 回転なし: X_cam = X_world + t。カメラ原点（X_cam = 0）は X_world = -t。
			name: "回転なし", qw: 1, t: [3]float64{1, 2, 3},
			wantCenter: [3]float64{-1, -2, -3}, wantDown: [3]float64{0, 1, 0},
		},
		{
			// X 軸まわりに 180°: R = diag(1, -1, -1)。カメラの「下」は世界の -Y（＝世界の Y が上）。
			// C = -Rᵀt = -(1, -2, -3) = (-1, 2, 3)
			name: "X軸まわり180度", qx: 1, t: [3]float64{1, 2, 3},
			wantCenter: [3]float64{-1, 2, 3}, wantDown: [3]float64{0, -1, 0},
		},
		{
			// Z 軸まわりに 90°（スマホを縦に構えた状態）: R = [[0,-1,0],[1,0,0],[0,0,1]]。
			// カメラの「下」(R の2行目) は世界の +X。 C = -Rᵀt = -(t_y, -t_x, t_z) = (-2, 1, -3)
			name: "Z軸まわり90度", qw: s, qz: s, t: [3]float64{1, 2, 3},
			wantCenter: [3]float64{-2, 1, -3}, wantDown: [3]float64{1, 0, 0},
		},
		{
			// 長さが1でない四元数（テキストの丸め誤差の極端な例）も正規化して扱う。
			name: "正規化されていない四元数", qw: 2, t: [3]float64{0, 0, 4},
			wantCenter: [3]float64{0, 0, -4}, wantDown: [3]float64{0, 1, 0},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := poseFromQuaternion(c.qw, c.qx, c.qy, c.qz, c.t)
			if !nearVec(got.Center, c.wantCenter) {
				t.Errorf("カメラ位置 = %v, want %v", got.Center, c.wantCenter)
			}
			if !nearVec(got.Down, c.wantDown) {
				t.Errorf("下方向 = %v, want %v", got.Down, c.wantDown)
			}
		})
	}
}

func writeModel(t *testing.T, images, points string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "images.txt"), []byte(images), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "points3D.txt"), []byte(points), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadSceneInfo(t *testing.T) {
	// COLMAP が書く形式をそのまま模したもの。
	//   - 3台のカメラ。全て「X軸まわり180°」＝世界の +Y が上になる向き。
	//   - 2台目は特徴点の行が空（空行でも1行として数える必要がある）。
	images := `# Image list with two lines of data per image:
#   IMAGE_ID, QW, QX, QY, QZ, TX, TY, TZ, CAMERA_ID, NAME
#   POINTS2D[] as (X, Y, POINT3D_ID)
# Number of images: 3, mean observations per image: 2
1 0 1 0 0 0 0 5 1 frame_0001.jpg
100.5 200.5 1 300.5 400.5 -1 500.0 600.0 2
2 0 1 0 0 -1 0 5 1 frame_0002.jpg

3 0 1 0 0 1 0 5 1 frame_0003.jpg
10 20 3
`
	// 点は原点付近に4つ＋遠くの外れ値1つ。中央値は外れ値に引っ張られない。
	points := `# 3D point list with one line of data per point:
#   POINT3D_ID, X, Y, Z, R, G, B, ERROR, TRACK[] as (IMAGE_ID, POINT2D_IDX)
# Number of points: 5, mean track length: 2
1 -1 0 0 255 0 0 0.5 1 0 2 0
2 1 0 0 0 255 0 1.5 1 1 3 0
3 0 -1 0 0 0 255 1.0 2 0 3 1
4 0 1 0 9 9 9 1.0 1 2 2 1
5 1000 1000 1000 9 9 9 1.0 1 3 2 2
`
	info, err := ReadSceneInfo(writeModel(t, images, points), 4)
	if err != nil {
		t.Fatal(err)
	}
	if info.TotalImages != 4 || info.RegisteredImages != 3 || info.Points != 5 {
		t.Errorf("枚数・点数 = %+v", info)
	}
	if !near(info.MeanReprojectionError, 1.0) {
		t.Errorf("平均再投影誤差 = %v, want 1.0", info.MeanReprojectionError)
	}
	// 全カメラの「下」が世界の -Y なので、「上」は +Y。
	if !nearVec(info.Up, [3]float64{0, 1, 0}) {
		t.Errorf("Up = %v, want (0,1,0)", info.Up)
	}
	// 中央値: x は {-1,0,0,1,1000} → 0、y も 0、z は {0,0,0,0,1000} → 0。
	if !nearVec(info.Center, [3]float64{0, 0, 0}) {
		t.Errorf("Center = %v, want 原点（外れ値に引っ張られない）", info.Center)
	}
	// 距離は {1,1,1,1,1732}。75パーセンタイル（5個中 index 3）は 1。
	if !near(info.Radius, 1) {
		t.Errorf("Radius = %v, want 1", info.Radius)
	}
	// カメラ位置: C = -Rᵀt。R = diag(1,-1,-1) なので t=(0,0,5) → (0,0,5)、t=(-1,0,5) → (1,0,5)。
	want := [][3]float64{{0, 0, 5}, {1, 0, 5}, {-1, 0, 5}}
	if len(info.CameraPositions) != 3 {
		t.Fatalf("カメラ数 = %d", len(info.CameraPositions))
	}
	for i := range want {
		if !nearVec(info.CameraPositions[i], want[i]) {
			t.Errorf("カメラ%d = %v, want %v", i, info.CameraPositions[i], want[i])
		}
	}
}

func TestReadSceneInfoErrors(t *testing.T) {
	if _, err := ReadSceneInfo(t.TempDir(), 3); err == nil {
		t.Error("ファイルが無いのにエラーにならない")
	}
	if _, err := ReadSceneInfo(writeModel(t, "# コメントだけ\n", "# 空\n"), 3); err == nil {
		t.Error("登録画像が0枚なのにエラーにならない")
	}
	if _, err := ReadSceneInfo(writeModel(t, "1 not a number\n\n", ""), 3); err == nil {
		t.Error("壊れた images.txt がエラーにならない")
	}
}

func TestWriteSceneInfoIsValidJSON(t *testing.T) {
	info := &SceneInfo{TotalImages: 3, RegisteredImages: 3, Points: 10, Up: [3]float64{0, 1, 0}, Radius: 2,
		CameraPositions: [][3]float64{{0, 0, 5}}}
	path := filepath.Join(t.TempDir(), "scene.json")
	if err := WriteSceneInfo(info, path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if b[0] != '{' {
		t.Fatalf("先頭が { でない: %q", b[:1])
	}
	var back SceneInfo
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.RegisteredImages != 3 || back.Up != info.Up || len(back.CameraPositions) != 1 {
		t.Errorf("読み戻した値が違う: %+v", back)
	}
}

// TestReadImagePosesWithObservations: 名前・カメラ ID・向き・3D点に対応づいた特徴点の数を読む。
func TestReadImagePosesWithObservations(t *testing.T) {
	dir := writeModel(t, "# Image list\n"+
		"# 2 lines per image\n"+
		"1 1 0 0 0 0 0 0 1 frame_0001.jpg\n"+
		"10.5 20.5 7 11 22 -1 30 40 8\n"+
		"2 1 0 0 0 1 0 0 2 vps query.jpg\n"+
		"\n", "")
	poses, err := ReadImagePoses(filepath.Join(dir, "images.txt"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(poses) != 2 {
		t.Fatalf("枚数 = %d", len(poses))
	}
	p := poses[0]
	if p.ID != 1 || p.CameraID != 1 || p.Name != "frame_0001.jpg" || p.Observations != 2 {
		t.Fatalf("1枚目 = %+v", p)
	}
	// 回転なしのカメラは +Z を向いている。
	if !nearVec(p.Forward, [3]float64{0, 0, 1}) {
		t.Fatalf("Forward = %v", p.Forward)
	}
	if q := poses[1]; q.Name != "vps query.jpg" || q.CameraID != 2 || q.Observations != 0 || !nearVec(q.Center, [3]float64{-1, 0, 0}) {
		t.Fatalf("2枚目 = %+v", q)
	}
	// 数えない指定なら 0 のまま。
	poses, _ = ReadImagePoses(filepath.Join(dir, "images.txt"), false)
	if poses[0].Observations != 0 {
		t.Fatalf("Observations = %d, want 0", poses[0].Observations)
	}
}

func TestReadCameras(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cameras.txt")
	os.WriteFile(path, []byte("# Camera list\n1 SIMPLE_RADIAL 1920 1440 1500 960 720 0.01\n2 PINHOLE 640 480 500 500 320 240\n"), 0o640)
	cams, err := ReadCameras(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cams) != 2 || cams[0].ID != 1 || cams[0].Model != "SIMPLE_RADIAL" || cams[0].Width != 1920 || cams[0].Height != 1440 || cams[1].ID != 2 {
		t.Fatalf("cams = %+v", cams)
	}
	// 焦点距離と主点: SIMPLE_RADIAL は f, cx, cy、PINHOLE は fx, fy, cx, cy。
	if in, ok := cams[0].Intrinsics(); !ok || in != (Intrinsics{Width: 1920, Height: 1440, Fx: 1500, Fy: 1500, Cx: 960, Cy: 720}) {
		t.Errorf("SIMPLE_RADIAL の画角 = %+v, %v", in, ok)
	}
	if in, ok := cams[1].Intrinsics(); !ok || in.Fx != 500 || in.Cy != 240 {
		t.Errorf("PINHOLE の画角 = %+v, %v", in, ok)
	}
	if _, ok := (Camera{Model: "UNKNOWN"}).Intrinsics(); ok {
		t.Error("知らないモデルの画角を返した")
	}
	os.WriteFile(path, []byte("1 SIMPLE_RADIAL x 1440\n"), 0o640)
	if _, err := ReadCameras(path); err == nil {
		t.Fatal("壊れた行を受け付けた")
	}
}

// ring は、点 target のまわりに半径 r で n 台のカメラを水平に並べ、全部が target を向く姿勢を作る。
func ring(target [3]float64, r float64, n int) []ImagePose {
	poses := make([]ImagePose, 0, n)
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		c := [3]float64{target[0] + r*math.Cos(a), target[1] + 0.5, target[2] + r*math.Sin(a)}
		f, _ := normalize([3]float64{target[0] - c[0], target[1] - c[1], target[2] - c[2]})
		poses = append(poses, ImagePose{Center: c, Forward: f})
	}
	return poses
}

// TestFocusPoint: 1周撮影では視線が集まる点を中心にし、視線が集まらない撮り方では使わないこと。
func TestFocusPoint(t *testing.T) {
	target := [3]float64{2, 1, 3}
	got, ok := focusPoint(ring(target, 4, 12))
	if !ok || !nearVec(got, target) {
		t.Errorf("1周: focusPoint = %v, %v, want %v", got, ok, target)
	}

	// 4分の1周（90°）でも、視線は十分に交わる。
	quarter := ring(target, 4, 36)[:10]
	if got, ok := focusPoint(quarter); !ok || !nearVec(got, target) {
		t.Errorf("4分の1周: focusPoint = %v, %v", got, ok)
	}

	// 壁に沿って歩いた（全部が同じ向き）: 点が決まらない。
	var wall []ImagePose
	for i := 0; i < 10; i++ {
		wall = append(wall, ImagePose{Center: [3]float64{float64(i), 0, 0}, Forward: [3]float64{0, 0, 1}})
	}
	if _, ok := focusPoint(wall); ok {
		t.Error("視線が平行なのに中心が決まった")
	}

	// 外向きに並んだカメラ（部屋の中から周りの壁を撮った）: 交点がカメラの後ろにあるので使わない。
	out := ring(target, 4, 12)
	for i := range out {
		out[i].Forward = [3]float64{-out[i].Forward[0], -out[i].Forward[1], -out[i].Forward[2]}
	}
	if _, ok := focusPoint(out); ok {
		t.Error("全カメラの後ろにある点を中心にした")
	}

	if _, ok := focusPoint(ring(target, 4, 2)); ok {
		t.Error("2台だけで中心を決めた")
	}
}
