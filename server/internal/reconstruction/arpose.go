package reconstruction

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// このファイルは、撮影側の AR（WebXR / ARCore）が測ったカメラの位置を使って、
// SfM の結果に「実際の大きさ」と「重力の向き」を与える。
//
// SfM の世界座標は原点・向き・縮尺が任意で、写真だけからは「1 が何メートルか」も「どちらが上か」も
// 分からない（sceneinfo.go の冒頭）。AR で撮った写真には、撮った瞬間のカメラの位置（メートル、y が上）が
// 付いている。同じ写真の SfM でのカメラ位置と並べれば、2つの座標系の関係
//
//	AR = s · R · SfM + t     （s: 縮尺、R: 回転、t: 平行移動）
//
// が解ける。s が「SfM の 1 が何メートルか」、Rᵀ·(0,1,0) が SfM の座標での真上になる。
//
// 解き方は Horn の方法（位置の組だけから s, R, t を最小二乗で求める）。カメラの向きは使わないので、
// カメラの軸の約束（WebXR は y 上・奥が -z、COLMAP は y 下・奥が +z）や、写真の上下の向きに左右されない。

// ARPose は AR が測った、写真1枚を撮ったときのカメラの位置と向き。
type ARPose struct {
	// Name は写真のファイル名（frame_0001.jpg）。
	Name string `json:"name"`
	// Session は AR を始めてから終えるまでの1回を表す印。AR の座標系は回ごとに違うので、回ごとに解く。
	Session string `json:"session"`
	// Position はカメラの位置（メートル、y が上）。
	Position [3]float64 `json:"position"`
	// Orientation はカメラの向き（四元数 x, y, z, w）。今は使わないが、後で向きも合わせるときのために残す。
	Orientation [4]float64 `json:"orientation"`
}

// ARAlignment は SfM の座標から AR の座標への変換（AR = Scale · Rotation · SfM + Translation）。
type ARAlignment struct {
	Session     string
	Images      int
	Scale       float64
	Rotation    [3][3]float64
	Translation [3]float64
	// RMS は合わせた後のカメラ位置のずれ（メートル、二乗平均の平方根）。
	RMS float64
	// Up は SfM の座標での真上（単位ベクトル）。
	Up [3]float64
}

const (
	// minARImages は合わせるのに要る写真の枚数の下限。
	minARImages = 5
	// minARSpread は、カメラが動いた範囲の下限（メートル）。2番目に広い方向の広がり（標準偏差）で見る。
	// 一直線に動いただけだと、その線のまわりの回転が決まらない。
	minARSpread = 0.05
	// maxARResidual は、合わせた後のずれの許容（メートルと、動いた範囲に対する割合）。
	maxARResidualMeters = 0.03
	maxARResidualRatio  = 0.1
)

func arPosesPath(ws *workspace.Scan) string { return filepath.Join(ws.Dir(), "ar_poses.json") }

// WriteARPoses は AR の位置を作業ディレクトリに書く（受付時）。
func WriteARPoses(ws *workspace.Scan, poses []ARPose) error {
	b, err := json.Marshal(poses)
	if err != nil {
		return err
	}
	return os.WriteFile(arPosesPath(ws), b, 0o640)
}

// ARPosesOf は、写真ごとのメタデータ（撮影側が送る JSON）の arPose から AR の位置を取り出す。
// 形がおかしいもの（数でない・とんでもなく遠い・回の印が無い）は使わない。
//
//	{"arPose": {"session": "…", "position": [x, y, z], "orientation": [x, y, z, w]}}
func ARPosesOf(frames []scan.Frame) []ARPose {
	var out []ARPose
	for _, f := range frames {
		var meta struct {
			ARPose *struct {
				Session     string     `json:"session"`
				Position    [3]float64 `json:"position"`
				Orientation [4]float64 `json:"orientation"`
			} `json:"arPose"`
		}
		if len(f.Camera) == 0 || json.Unmarshal(f.Camera, &meta) != nil || meta.ARPose == nil {
			continue
		}
		a := meta.ARPose
		if a.Session == "" || len(a.Session) > 64 || !finite3(a.Position) || dot3(a.Position, a.Position) > 1e6 {
			continue
		}
		out = append(out, ARPose{Name: f.FileName, Session: a.Session, Position: a.Position, Orientation: a.Orientation})
	}
	return out
}

// ReadARPoses は AR の位置を読む。無い・読めないときは nil。
func ReadARPoses(ws *workspace.Scan) []ARPose {
	b, err := os.ReadFile(arPosesPath(ws))
	if err != nil {
		return nil
	}
	var poses []ARPose
	if json.Unmarshal(b, &poses) != nil {
		return nil
	}
	return poses
}

// AlignToAR は SfM のカメラ位置と AR のカメラ位置を突き合わせ、座標の変換を求める。
// AR の回が複数あれば回ごとに解き、条件を満たしたうち写真の多い回を使う。
// 合わせられない（枚数・動いた範囲が足りない、ずれが大きい）ときは ok=false。
func AlignToAR(poses []ImagePose, ar []ARPose) (ARAlignment, bool) {
	sfmByName := make(map[string][3]float64, len(poses))
	for _, p := range poses {
		sfmByName[p.Name] = p.Center
	}
	type pair struct{ sfm, ar [3]float64 }
	bySession := map[string][]pair{}
	for _, a := range ar {
		if c, ok := sfmByName[a.Name]; ok && finite3(a.Position) {
			bySession[a.Session] = append(bySession[a.Session], pair{c, a.Position})
		}
	}
	sessions := make([]string, 0, len(bySession))
	for s := range bySession {
		sessions = append(sessions, s)
	}
	sort.Strings(sessions) // 同じ枚数のときに結果が毎回同じになるように

	var best ARAlignment
	found := false
	for _, s := range sessions {
		pairs := bySession[s]
		if len(pairs) < minARImages || (found && len(pairs) <= best.Images) {
			continue
		}
		src := make([][3]float64, len(pairs))
		dst := make([][3]float64, len(pairs))
		for i, p := range pairs {
			src[i], dst[i] = p.sfm, p.ar
		}
		al, ok := similarity(src, dst)
		if !ok {
			continue
		}
		al.Session = s
		best, found = al, true
	}
	return best, found
}

// similarity は dst ≈ s·R·src + t となる s, R, t を求める（Horn の方法）。
func similarity(src, dst [][3]float64) (ARAlignment, bool) {
	n := float64(len(src))
	cs, cd := mean3(src), mean3(dst)
	// 重心を引いた位置と、相関行列 S[a][b] = Σ src'_a · dst'_b。
	var S [3][3]float64
	var srcNorm float64
	for i := range src {
		a := sub3(src[i], cs)
		b := sub3(dst[i], cd)
		srcNorm += dot3(a, a)
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				S[r][c] += a[r] * b[c]
			}
		}
	}
	if srcNorm <= 0 {
		return ARAlignment{}, false
	}
	// AR 側の広がり: 2番目に広い方向の標準偏差が minARSpread 以上（一直線・その場で回っただけを除く）。
	if spread := secondSpread(dst, cd); spread < minARSpread {
		return ARAlignment{}, false
	}

	// 回転を表す四元数は、次の対称行列の最大固有値の固有ベクトル（Horn 1987）。
	N := [4][4]float64{
		{S[0][0] + S[1][1] + S[2][2], S[1][2] - S[2][1], S[2][0] - S[0][2], S[0][1] - S[1][0]},
		{S[1][2] - S[2][1], S[0][0] - S[1][1] - S[2][2], S[0][1] + S[1][0], S[2][0] + S[0][2]},
		{S[2][0] - S[0][2], S[0][1] + S[1][0], -S[0][0] + S[1][1] - S[2][2], S[1][2] + S[2][1]},
		{S[0][1] - S[1][0], S[2][0] + S[0][2], S[1][2] + S[2][1], -S[0][0] - S[1][1] + S[2][2]},
	}
	q := maxEigenvector4(N)
	R := quatToMatrix(q[0], q[1], q[2], q[3])

	// 縮尺: Σ dst'·(R src') / Σ |src'|²。
	var num float64
	for i := range src {
		num += dot3(sub3(dst[i], cd), mulMat3(R, sub3(src[i], cs)))
	}
	s := num / srcNorm
	if !(s > 0) || math.IsInf(s, 0) {
		return ARAlignment{}, false
	}
	rs := mulMat3(R, cs)
	t := [3]float64{cd[0] - s*rs[0], cd[1] - s*rs[1], cd[2] - s*rs[2]}

	var sq, spread float64
	for i := range src {
		p := mulMat3(R, src[i])
		e := sub3(dst[i], [3]float64{s*p[0] + t[0], s*p[1] + t[1], s*p[2] + t[2]})
		sq += dot3(e, e)
		d := sub3(dst[i], cd)
		spread += dot3(d, d)
	}
	rms := math.Sqrt(sq / n)
	if rms > maxARResidualMeters+maxARResidualRatio*math.Sqrt(spread/n) {
		return ARAlignment{}, false
	}
	// SfM での真上 = Rᵀ·(0,1,0) = R の2行目。
	up, ok := normalize([3]float64{R[1][0], R[1][1], R[1][2]})
	if !ok {
		return ARAlignment{}, false
	}
	return ARAlignment{Images: len(src), Scale: s, Rotation: R, Translation: t, RMS: rms, Up: up}, true
}

// secondSpread は点の散らばり（共分散）の2番目に大きい固有値の平方根（標準偏差、メートル）。
func secondSpread(points [][3]float64, c [3]float64) float64 {
	var C [4][4]float64 // 3x3 を 4x4 の左上に置き、固有値の計算を使い回す
	for _, p := range points {
		d := sub3(p, c)
		for r := 0; r < 3; r++ {
			for k := 0; k < 3; k++ {
				C[r][k] += d[r] * d[k] / float64(len(points))
			}
		}
	}
	vals, _ := jacobi4(C)
	v := []float64{vals[0], vals[1], vals[2], vals[3]}
	sort.Float64s(v)
	// 4つのうち1つは足した 0 の行列の分なので、上から2番目が 3x3 の2番目。
	return math.Sqrt(math.Max(0, v[2]))
}

// maxEigenvector4 は対称行列の最大固有値の固有ベクトル（単位ベクトル）。
func maxEigenvector4(m [4][4]float64) [4]float64 {
	vals, vecs := jacobi4(m)
	best := 0
	for i := 1; i < 4; i++ {
		if vals[i] > vals[best] {
			best = i
		}
	}
	return [4]float64{vecs[0][best], vecs[1][best], vecs[2][best], vecs[3][best]}
}

// jacobi4 は 4x4 の対称行列の固有値と固有ベクトル（列）を、ヤコビ法で求める。
func jacobi4(a [4][4]float64) ([4]float64, [4][4]float64) {
	var v [4][4]float64
	for i := 0; i < 4; i++ {
		v[i][i] = 1
	}
	for sweep := 0; sweep < 50; sweep++ {
		var off float64
		for p := 0; p < 4; p++ {
			for q := p + 1; q < 4; q++ {
				off += a[p][q] * a[p][q]
			}
		}
		if off < 1e-30 {
			break
		}
		for p := 0; p < 4; p++ {
			for q := p + 1; q < 4; q++ {
				if math.Abs(a[p][q]) < 1e-300 {
					continue
				}
				theta := (a[q][q] - a[p][p]) / (2 * a[p][q])
				t := math.Copysign(1, theta) / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < 4; k++ {
					akp, akq := a[k][p], a[k][q]
					a[k][p], a[k][q] = c*akp-s*akq, s*akp+c*akq
				}
				for k := 0; k < 4; k++ {
					apk, aqk := a[p][k], a[q][k]
					a[p][k], a[q][k] = c*apk-s*aqk, s*apk+c*aqk
				}
				for k := 0; k < 4; k++ {
					vkp, vkq := v[k][p], v[k][q]
					v[k][p], v[k][q] = c*vkp-s*vkq, s*vkp+c*vkq
				}
			}
		}
	}
	return [4]float64{a[0][0], a[1][1], a[2][2], a[3][3]}, v
}

// quatToMatrix は単位四元数 (w, x, y, z) を回転行列にする。
func quatToMatrix(w, x, y, z float64) [3][3]float64 {
	l := math.Sqrt(w*w + x*x + y*y + z*z)
	w, x, y, z = w/l, x/l, y/l, z/l
	return [3][3]float64{
		{1 - 2*(y*y+z*z), 2 * (x*y - w*z), 2 * (x*z + w*y)},
		{2 * (x*y + w*z), 1 - 2*(x*x+z*z), 2 * (y*z - w*x)},
		{2 * (x*z - w*y), 2 * (y*z + w*x), 1 - 2*(x*x+y*y)},
	}
}

func mulMat3(m [3][3]float64, v [3]float64) [3]float64 {
	return [3]float64{
		m[0][0]*v[0] + m[0][1]*v[1] + m[0][2]*v[2],
		m[1][0]*v[0] + m[1][1]*v[1] + m[1][2]*v[2],
		m[2][0]*v[0] + m[2][1]*v[1] + m[2][2]*v[2],
	}
}

func mean3(points [][3]float64) [3]float64 {
	var m [3]float64
	for _, p := range points {
		m[0], m[1], m[2] = m[0]+p[0], m[1]+p[1], m[2]+p[2]
	}
	n := float64(len(points))
	return [3]float64{m[0] / n, m[1] / n, m[2] / n}
}

func sub3(a, b [3]float64) [3]float64 { return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func dot3(a, b [3]float64) float64    { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func finite3(v [3]float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}
