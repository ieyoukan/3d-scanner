package reconstruction

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// このファイルは「風景モード」（物を囲まずに、歩きながら空間を撮る）のための SfM の工夫をまとめる。
//
// 物を囲んで撮る（既定）と違うところ:
//
//	枚数     空間は広いので写真が多い（MAX_SCENE_IMAGES 枚まで）。
//	照合     全部の組（N(N-1)/2）を照合すると枚数の二乗で遅くなる（CPU で 1 組 0.25〜0.5 秒、150 枚で 1 万組を超える）。
//	         歩きながら撮った写真は、撮った順に近いものどうしが重なっている。そこで次の組だけを照合する（ScenePairs）:
//	           - 撮った順で前後 sceneOverlap 枚
//	           - 撮った順で 16, 32, 64, 128 枚離れたもの（1周して戻ってきたときに、最初の方とつなぐため）
//	           - AR で撮っていれば、AR が測った位置が近い sceneARNeighbors 枚（同じ所を2回通ったときにつなぐ）
//	           - 続きの版では、今回の各写真と、引き継いだ写真から等間隔に選んだ sceneBaseSamples 枚
//	失敗の伝え方  空間の撮影は途中で途切れやすい（速く動いた・暗い・白い壁）。どの写真がつながらなかったかを
//	         「◯〜◯枚目」で伝える（UnregisteredRuns）。

// CaptureMode は撮り方。
type CaptureMode string

const (
	// ModeObject は物を1つ囲んで撮る（既定）。
	ModeObject CaptureMode = "object"
	// ModeScene は歩きながら空間（部屋・庭・街並み）を撮る。
	ModeScene CaptureMode = "scene"
)

// ParseCaptureMode は文字列を撮り方にする。空なら物。知らない値なら ok=false。
func ParseCaptureMode(s string) (CaptureMode, bool) {
	switch CaptureMode(s) {
	case "", ModeObject:
		return ModeObject, true
	case ModeScene:
		return ModeScene, true
	}
	return "", false
}

func captureModePath(ws *workspace.Scan) string { return filepath.Join(ws.Dir(), "capture_mode") }

// WriteCaptureMode は撮り方を作業ディレクトリに書く（受付時）。物なら何も書かない。
func WriteCaptureMode(ws *workspace.Scan, m CaptureMode) error {
	if m != ModeScene {
		return nil
	}
	return os.WriteFile(captureModePath(ws), []byte(m), 0o640)
}

// ReadCaptureMode は撮り方を読む。無い・読めないときは物。
func ReadCaptureMode(ws *workspace.Scan) CaptureMode {
	b, err := os.ReadFile(captureModePath(ws))
	if err != nil {
		return ModeObject
	}
	if m, ok := ParseCaptureMode(strings.TrimSpace(string(b))); ok {
		return m
	}
	return ModeObject
}

const (
	sceneOverlap     = 8
	sceneARNeighbors = 5
	sceneBaseSamples = 30
)

// sceneLongJumps は撮った順で離れた写真どうしを照合する間隔（1周して戻ってきたときにつなぐ）。
var sceneLongJumps = []int{16, 32, 64, 128}

// ScenePairs は風景モードで照合する写真の組を返す（matches_importer の pairs 形式に使う）。
//
// 入力:
//   - names: 写真のファイル名（撮った順。frame_0001.jpg … は撮った順に番号が付いている）
//   - inherited: 続きの版で引き継いだ写真（今回の写真どうしの組は作らず、今回 × 引き継ぎの組だけ作る）
//   - ar: AR が測ったカメラの位置（無くてもよい）
//
// 出力: 重複の無い組（各組は名前の順に並べる）。
func ScenePairs(names []string, inherited map[string]bool, ar []ARPose) [][2]string {
	var fresh, base []string
	for _, n := range names {
		if inherited[n] {
			base = append(base, n)
		} else {
			fresh = append(fresh, n)
		}
	}
	seen := map[[2]string]bool{}
	var pairs [][2]string
	add := func(a, b string) {
		if a == b {
			return
		}
		if b < a {
			a, b = b, a
		}
		k := [2]string{a, b}
		if !seen[k] {
			seen[k] = true
			pairs = append(pairs, k)
		}
	}

	// 撮った順で近いもの・1周分離れたもの（今回の写真どうし）。
	for i := range fresh {
		for d := 1; d <= sceneOverlap && i+d < len(fresh); d++ {
			add(fresh[i], fresh[i+d])
		}
		for _, d := range sceneLongJumps {
			if i+d < len(fresh) {
				add(fresh[i], fresh[i+d])
			}
		}
	}

	// AR の位置が近いもの（同じ回の中だけ。回が違うと座標系が違う）。今回の写真を含む組だけ。
	isFresh := map[string]bool{}
	for _, n := range fresh {
		isFresh[n] = true
	}
	present := map[string]bool{}
	for _, n := range names {
		present[n] = true
	}
	bySession := map[string][]ARPose{}
	for _, a := range ar {
		if present[a.Name] {
			bySession[a.Session] = append(bySession[a.Session], a)
		}
	}
	for _, poses := range bySession {
		for _, p := range poses {
			if !isFresh[p.Name] {
				continue
			}
			for _, q := range nearestARPoses(p, poses, sceneARNeighbors) {
				add(p.Name, q.Name)
			}
		}
	}

	// 続きの版: 今回の各写真 × 引き継いだ写真から等間隔に選んだもの。
	for _, b := range sampleEvenly(base, sceneBaseSamples) {
		for _, f := range fresh {
			add(f, b)
		}
	}
	return pairs
}

// nearestARPoses は p に位置が近く、だいたい同じ側（視線のなす角が 90° 未満）を向いている写真を n 枚返す。
func nearestARPoses(p ARPose, poses []ARPose, n int) []ARPose {
	fp := arForward(p.Orientation)
	type cand struct {
		pose ARPose
		d    float64
	}
	var cs []cand
	for _, q := range poses {
		if q.Name == p.Name {
			continue
		}
		// 向きが分かっているときだけ、反対側を向いた写真（同じ所でも写っている物が違う）を除く。
		if fq := arForward(q.Orientation); fp != ([3]float64{}) && fq != ([3]float64{}) && dot3(fp, fq) <= 0 {
			continue
		}
		cs = append(cs, cand{q, math.Sqrt(dot3(sub3(q.Position, p.Position), sub3(q.Position, p.Position)))})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].d < cs[j].d })
	out := make([]ARPose, 0, n)
	for i := 0; i < len(cs) && i < n; i++ {
		out = append(out, cs[i].pose)
	}
	return out
}

// arForward は AR のカメラの向き（四元数 x, y, z, w）から視線（カメラの -z）を求める。
// 四元数が無い（ゼロ）ときは (0,0,0) を返す（呼び出し側は向きで絞らない）。
func arForward(q [4]float64) [3]float64 {
	x, y, z, w := q[0], q[1], q[2], q[3]
	if x == 0 && y == 0 && z == 0 && w == 0 {
		return [3]float64{}
	}
	// 回転行列の第3列の符号を変えたもの（R·(0,0,-1)）。
	return [3]float64{-(2 * (x*z + w*y)), -(2 * (y*z - w*x)), -(1 - 2*(x*x+y*y))}
}

func sampleEvenly(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, items[i*len(items)/n])
	}
	return out
}

// WritePairs は組を matches_importer の pairs 形式（1行に「名前 名前」）で書く。
func WritePairs(path string, pairs [][2]string) error {
	var b strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&b, "%s %s\n", p[0], p[1])
	}
	return os.WriteFile(path, []byte(b.String()), 0o640)
}

// frameNumberPattern は写真のファイル名（frame_0041.jpg）から番号を取り出す。
var frameNumberPattern = regexp.MustCompile(`^frame_(\d+)\.`)

// UnregisteredRuns は、位置を推定できなかった写真を、撮った順の番号のひと続きにまとめる（[始め, 終わり]）。
// 番号の付いていない名前は数えない。
func UnregisteredRuns(all []string, registered []string) [][2]int {
	ok := map[string]bool{}
	for _, n := range registered {
		ok[n] = true
	}
	var missing []int
	for _, n := range all {
		if ok[n] {
			continue
		}
		if m := frameNumberPattern.FindStringSubmatch(n); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				missing = append(missing, v)
			}
		}
	}
	sort.Ints(missing)
	var runs [][2]int
	for _, v := range missing {
		if len(runs) > 0 && runs[len(runs)-1][1] == v-1 {
			runs[len(runs)-1][1] = v
		} else {
			runs = append(runs, [2]int{v, v})
		}
	}
	return runs
}

// describeRuns は UnregisteredRuns を「41〜55、60枚目」のような文にする（多ければ先頭の数か所だけ）。
func describeRuns(runs [][2]int) string {
	const maxShown = 4
	parts := make([]string, 0, maxShown)
	for i, r := range runs {
		if i == maxShown {
			parts = append(parts, fmt.Sprintf("ほか%dか所", len(runs)-maxShown))
			break
		}
		if r[0] == r[1] {
			parts = append(parts, strconv.Itoa(r[0]))
		} else {
			parts = append(parts, fmt.Sprintf("%d〜%d", r[0], r[1]))
		}
	}
	return strings.Join(parts, "、") + "枚目"
}

// listImageNames は images/ の写真の名前を、名前の順（＝撮った順）に返す。
func listImageNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".jpg", ".jpeg", ".png":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
