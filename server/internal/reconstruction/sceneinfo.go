package reconstruction

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// このファイルは SfM の結果（COLMAP のテキスト形式）を読み、
// 「何枚の位置が分かったか」と「ビューアの初期視点に使う情報」を取り出す。
//
// COLMAP の座標系のおさらい（docs/learning/02-sfm.md に図あり）:
//
//   - 世界座標: SfM が勝手に決めた座標系。原点・向き・縮尺に物理的な意味は無い
//     （写真だけからは「1 が何メートルか」も「どちらが上か」も分からない）。
//   - カメラ座標: カメラから見て X が右、Y が下、Z が奥（レンズの向く方向）。
//   - images.txt の1行は「世界座標 → カメラ座標」の変換を持つ:
//     X_cam = R * X_world + t      （R は四元数 QW QX QY QZ、t は TX TY TZ）
//
// ここから導けるもの:
//
//   - カメラの位置（世界座標）: X_cam = 0 となる点なので  C = -Rᵀ t
//   - カメラの「下」方向（世界座標）: Rᵀ (0,1,0)ᵀ ＝ R の2行目
//   - カメラの「奥」方向（世界座標）: Rᵀ (0,0,1)ᵀ ＝ R の3行目
//
// 人は普通スマホをほぼ水平に構えるので、全カメラの「下」方向を平均すると重力の向きに近い。
// その逆を「上」としてビューアに渡せば、原点も向きも任意な SfM の結果を自然な向きで表示できる。

// SceneInfo は SfM 結果の要約。output/scene.json として保存し、ビューアが読む。
type SceneInfo struct {
	// TotalImages は入力した写真の枚数、RegisteredImages はカメラ位置を推定できた枚数。
	TotalImages      int `json:"totalImages"`
	RegisteredImages int `json:"registeredImages"`
	// Points は疎な点群の点数。
	Points int `json:"points"`
	// MeanReprojectionError は再投影誤差の平均（画素）。
	// 3D点をカメラに投影し直した位置と、実際に写真で見つかった位置のずれ。小さいほど整合している。
	MeanReprojectionError float64 `json:"meanReprojectionError"`

	// Up は世界座標での「上」方向（単位ベクトル）。
	Up [3]float64 `json:"up"`
	// Center は被写体の中心の推定値。全カメラの視線が集まる点（focusPoint）。
	// 視線が集まらない撮り方（壁に沿って歩いた等）では、疎な点群の座標ごとの中央値。
	Center [3]float64 `json:"center"`
	// Radius は中心から点までの距離の 75 パーセンタイル。表示範囲の目安。
	Radius float64 `json:"radius"`
	// CameraPositions は各カメラの位置。初期視点を「撮影した側」に置くために使う。
	CameraPositions [][3]float64 `json:"cameraPositions"`
	// MetersPerUnit は SfM の座標の 1 が何メートルか。AR で撮った写真の位置から求められたときだけ入る
	// （arpose.go）。そのときは Up も、カメラの向きの平均ではなく AR が測った重力の向きになる。
	MetersPerUnit float64 `json:"metersPerUnit,omitempty"`

	// names は位置を推定できた写真のファイル名（CameraPositions と同じ順）。
	// 続きの版で「今回の写真が何枚入ったか」を数えるのに使う。scene.json には書かない。
	names []string
	// poses は位置を推定できた写真のカメラ。AR の位置との突き合わせに使う。scene.json には書かない。
	poses []ImagePose
}

// ImagePose は images.txt から読んだ写真1枚ぶんのカメラの位置と向き（世界座標）。
type ImagePose struct {
	ID       int
	CameraID int
	// Name は写真のファイル名（images/ からの相対パス）。
	Name string
	// Center はカメラの位置。
	Center [3]float64
	// Down はカメラの「下」（画像の下方向）、Forward は「奥」（レンズの向く方向）。どちらも単位ベクトル。
	Down    [3]float64
	Forward [3]float64
	// Observations は、この写真の特徴点のうち3D点に対応づいたものの数。
	// ReadImagePoses を withObservations=true で呼んだときだけ数える（行が長く、数えるのに時間がかかるため）。
	Observations int
}

// ReadSceneInfo は COLMAP のテキスト形式モデル（images.txt と points3D.txt）を読んで要約を作る。
//
// 入力: model_converter --output_type TXT の出力ディレクトリと、入力写真の枚数。
// 出力: SceneInfo。
// 失敗条件: ファイルが無い・形式が違う・登録された画像が0枚。
func ReadSceneInfo(txtDir string, totalImages int) (*SceneInfo, error) {
	poses, err := ReadImagePoses(filepath.Join(txtDir, "images.txt"), false)
	if err != nil {
		return nil, fmt.Errorf("images.txt を読めません: %w", err)
	}
	if len(poses) == 0 {
		return nil, errors.New("位置を推定できた写真が1枚もありません")
	}
	points, meanErr, err := readPoints3DTxt(filepath.Join(txtDir, "points3D.txt"))
	if err != nil {
		return nil, fmt.Errorf("points3D.txt を読めません: %w", err)
	}

	info := &SceneInfo{
		TotalImages:           totalImages,
		RegisteredImages:      len(poses),
		Points:                len(points),
		poses:                 poses,
		MeanReprojectionError: meanErr,
		Up:                    [3]float64{0, -1, 0}, // 求められないときの既定値（カメラ座標の上）
	}

	var downSum [3]float64
	for _, p := range poses {
		info.CameraPositions = append(info.CameraPositions, p.Center)
		info.names = append(info.names, p.Name)
		for k := 0; k < 3; k++ {
			downSum[k] += p.Down[k]
		}
	}
	if up, ok := normalize([3]float64{-downSum[0], -downSum[1], -downSum[2]}); ok {
		info.Up = up
	}

	if len(points) > 0 {
		info.Center = medianPoint(points)
	} else {
		// 点が無い（ほぼ起きない）ときはカメラ位置の中央値を中心にする。
		info.Center = medianPoint(info.CameraPositions)
	}
	// 対象のまわりを1周撮ったなら、被写体はカメラの視線が集まる所にある。
	// 点の中央値は、模様があって点の多い床や背景に引っ張られる（1周の動画3本で、被写体から
	// カメラまでの距離の 0.22〜0.95 倍ずれ、ビューアが床を見下ろす向きで開いていた）。
	if focus, ok := focusPoint(poses); ok {
		info.Center = focus
	}
	if len(points) > 0 {
		info.Radius = percentileDistance(points, info.Center, 0.75)
	}
	if info.Radius <= 0 || math.IsNaN(info.Radius) {
		info.Radius = 1
	}
	return info, nil
}

// focusPoint は、全カメラの視線（カメラ位置から「奥」へ伸びる直線）に最も近い点を返す。
//
// 求め方（最小二乗）: 点 x から視線 i までの距離の2乗は |(I − dᵢdᵢᵀ)(x − cᵢ)|²
// （cᵢ はカメラ位置、dᵢ は視線の向き。I − ddᵀ は「視線に垂直な成分だけを残す」射影）。
// これを全カメラで足して x で微分すると、3元の連立方程式  (Σ Pᵢ) x = Σ Pᵢ cᵢ  （Pᵢ = I − dᵢdᵢᵀ）になる。
//
// 使えない場合は ok = false:
//   - 視線がほぼ平行（壁に沿って歩いた、同じ方向ばかり撮った）。方程式の行列がほぼ特異になり、
//     点が1つに決まらない。行列を枚数で割ったものの行列式で判定する（1周なら約 0.27〜0.30、
//     平行なら 0）。
//   - 求めた点が、カメラの2割より多くの「後ろ」にある（被写体を囲んでいない）。
func focusPoint(poses []ImagePose) ([3]float64, bool) {
	const minPoses = 3
	const minDet = 0.02
	const minInFront = 0.8
	if len(poses) < minPoses {
		return [3]float64{}, false
	}
	var a [3][3]float64
	var b [3]float64
	for _, p := range poses {
		d := p.Forward
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				m := -d[i] * d[j]
				if i == j {
					m++
				}
				a[i][j] += m
				b[i] += m * p.Center[j]
			}
		}
	}
	n := float64(len(poses))
	var an [3][3]float64
	for i := range a {
		for j := range a[i] {
			an[i][j] = a[i][j] / n
		}
	}
	if det3(an) < minDet {
		return [3]float64{}, false
	}
	x, ok := solve3(a, b)
	if !ok {
		return [3]float64{}, false
	}
	front := 0
	for _, p := range poses {
		if (x[0]-p.Center[0])*p.Forward[0]+(x[1]-p.Center[1])*p.Forward[1]+(x[2]-p.Center[2])*p.Forward[2] > 0 {
			front++
		}
	}
	if float64(front) < minInFront*n {
		return [3]float64{}, false
	}
	return x, true
}

func det3(m [3][3]float64) float64 {
	return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
}

// solve3 は 3元連立一次方程式 m x = v をクラメルの公式で解く。
func solve3(m [3][3]float64, v [3]float64) ([3]float64, bool) {
	d := det3(m)
	if d == 0 || math.IsNaN(d) || math.IsInf(d, 0) {
		return [3]float64{}, false
	}
	var x [3]float64
	for k := 0; k < 3; k++ {
		mk := m
		for i := 0; i < 3; i++ {
			mk[i][k] = v[i]
		}
		x[k] = det3(mk) / d
	}
	for _, c := range x {
		if math.IsNaN(c) || math.IsInf(c, 0) {
			return [3]float64{}, false
		}
	}
	return x, true
}

// WriteSceneInfo は SceneInfo を JSON ファイルに書く。
func WriteSceneInfo(info *SceneInfo, path string) error {
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o640)
}

// ReadImagePoses は images.txt を読み、写真ごとのカメラの位置と向きを返す。
//
// 形式（COLMAP ドキュメント "Output Format" より）: 1画像につき2行。
//
//	IMAGE_ID QW QX QY QZ TX TY TZ CAMERA_ID NAME
//	X Y POINT3D_ID  X Y POINT3D_ID ...   ← その画像で見つかった特徴点（非常に長い行。空のこともある）
//
// 2行目は withObservations が true のときだけ解釈する（3D点に対応づいた特徴点を数える）。
// どちらの場合も「空行でも1行と数える」ことに注意して読み進める
// （空行を無視すると、次の画像の1行目を2行目と取り違える）。
func ReadImagePoses(path string, withObservations bool) ([]ImagePose, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)

	var poses []ImagePose
	for {
		line, err := readLongLine(r)
		if errors.Is(err, io.EOF) {
			return poses, nil
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue // ヘッダと最初の画像の間の空行、または末尾の空行
		}
		if len(fields) < 10 {
			return nil, fmt.Errorf("画像の行の項目が足りません: %q", truncate(line, 80))
		}
		nums, err := parseFloats(fields[1:8])
		if err != nil {
			return nil, err
		}
		pose := poseFromQuaternion(nums[0], nums[1], nums[2], nums[3], [3]float64{nums[4], nums[5], nums[6]})
		pose.ID, _ = strconv.Atoi(fields[0])
		pose.CameraID, _ = strconv.Atoi(fields[8])
		// 名前に空白が含まれていても読めるよう、9番目以降をつなげる。
		pose.Name = strings.Join(fields[9:], " ")

		// 特徴点の行（空でも必ず1行ある）。
		points, err := readLongLine(r)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if withObservations {
			pose.Observations = countObservations(points)
		}
		poses = append(poses, pose)
	}
}

// countObservations は特徴点の行（"X Y POINT3D_ID" の繰り返し）のうち、
// 3D点に対応づいた（POINT3D_ID が -1 でない）ものを数える。
func countObservations(line string) int {
	fields := strings.Fields(line)
	n := 0
	for i := 2; i < len(fields); i += 3 {
		if fields[i] != "-1" {
			n++
		}
	}
	return n
}

// Camera は cameras.txt の1行（カメラの内部パラメータ）。
type Camera struct {
	ID     int
	Model  string
	Width  int
	Height int
}

// ReadCameras は cameras.txt を読む。形式: CAMERA_ID MODEL WIDTH HEIGHT PARAMS[]
func ReadCameras(path string) ([]Camera, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cams []Camera
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("カメラの行の項目が足りません: %q", truncate(line, 80))
		}
		id, err1 := strconv.Atoi(fields[0])
		w, err2 := strconv.Atoi(fields[2])
		h, err3 := strconv.Atoi(fields[3])
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, fmt.Errorf("カメラの行を読めません: %q", truncate(line, 80))
		}
		cams = append(cams, Camera{ID: id, Model: fields[1], Width: w, Height: h})
	}
	return cams, nil
}

// poseFromQuaternion は四元数と並進からカメラの位置と向きを求める。
func poseFromQuaternion(qw, qx, qy, qz float64, t [3]float64) ImagePose {
	// 四元数は長さ1のはずだが、テキストに丸めて書かれているので正規化し直す。
	n := math.Sqrt(qw*qw + qx*qx + qy*qy + qz*qz)
	if n == 0 {
		n = 1
	}
	qw, qx, qy, qz = qw/n, qx/n, qy/n, qz/n

	// 単位四元数 → 回転行列（世界 → カメラ）。R[i] が i 行目。
	R := [3][3]float64{
		{1 - 2*(qy*qy+qz*qz), 2 * (qx*qy - qz*qw), 2 * (qx*qz + qy*qw)},
		{2 * (qx*qy + qz*qw), 1 - 2*(qx*qx+qz*qz), 2 * (qy*qz - qx*qw)},
		{2 * (qx*qz - qy*qw), 2 * (qy*qz + qx*qw), 1 - 2*(qx*qx+qy*qy)},
	}
	// C = -Rᵀ t。Rᵀ t の k 成分は「R の k 列目と t の内積」。
	var c [3]float64
	for k := 0; k < 3; k++ {
		c[k] = -(R[0][k]*t[0] + R[1][k]*t[1] + R[2][k]*t[2])
	}
	// カメラの Y 軸（画像の下方向）を世界座標で表すと R の2行目、Z 軸（奥）は3行目。
	return ImagePose{Center: c, Down: R[1], Forward: R[2]}
}

// readPoints3DTxt は points3D.txt から点の座標と再投影誤差の平均を読む。
//
// 形式: POINT3D_ID X Y Z R G B ERROR TRACK[]...
func readPoints3DTxt(path string) (points [][3]float64, meanError float64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)

	var errSum float64
	for {
		line, err := readLongLine(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		// 行の後半（TRACK）は長いので、先頭の8項目だけを切り出す。
		fields := strings.SplitN(strings.TrimSpace(line), " ", 9)
		if len(fields) < 8 {
			continue
		}
		xyz, err := parseFloats(fields[1:4])
		if err != nil {
			return nil, 0, err
		}
		e, err := strconv.ParseFloat(fields[7], 64)
		if err != nil {
			return nil, 0, err
		}
		points = append(points, [3]float64{xyz[0], xyz[1], xyz[2]})
		errSum += e
	}
	if len(points) > 0 {
		meanError = errSum / float64(len(points))
	}
	return points, meanError, nil
}

// readLongLine は長さに上限なく1行読む（images.txt の特徴点の行は数MBになる）。
func readLongLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func parseFloats(fields []string) ([]float64, error) {
	out := make([]float64, len(fields))
	for i, s := range fields {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("数値として読めません: %q", s)
		}
		out[i] = v
	}
	return out, nil
}

func normalize(v [3]float64) ([3]float64, bool) {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if n < 1e-9 || math.IsNaN(n) {
		return v, false
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}, true
}

// medianPoint は座標ごとの中央値を返す。
// 平均ではなく中央値を使うのは、SfM の点群には遠くの背景や誤対応による外れ値が混ざり、
// 平均だと中心がそちらに引っ張られるため。
func medianPoint(points [][3]float64) [3]float64 {
	var out [3]float64
	vals := make([]float64, len(points))
	for k := 0; k < 3; k++ {
		for i, p := range points {
			vals[i] = p[k]
		}
		sort.Float64s(vals)
		out[k] = vals[len(vals)/2]
	}
	return out
}

// percentileDistance は center から各点までの距離を小さい順に並べ、q（0〜1）の位置の値を返す。
func percentileDistance(points [][3]float64, center [3]float64, q float64) float64 {
	dists := make([]float64, len(points))
	for i, p := range points {
		dx, dy, dz := p[0]-center[0], p[1]-center[1], p[2]-center[2]
		dists[i] = math.Sqrt(dx*dx + dy*dy + dz*dz)
	}
	sort.Float64s(dists)
	idx := int(q * float64(len(dists)-1))
	return dists[idx]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
