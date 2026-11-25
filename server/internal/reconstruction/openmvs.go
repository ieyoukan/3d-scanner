package reconstruction

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/cmdrun"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
)

// このファイルは、OpenMVS で密なメッシュを作る分岐（MESH_MODE=openmvs、または auto で GPU が無く OpenMVS があるとき）。
//
// COLMAP の密な復元（patch_match_stereo）は CUDA 必須なので、GPU の無いサーバーでは疎な点群からの簡易メッシュしか
// 作れなかった（細い棒が消える、白い天板が平らになる、背景の膜が広がる）。OpenMVS の密な復元は CPU で動く。
// 1 周の動画（31 枚）で、面は 9,507 → 約 41 万、消えていた棒と上の看板が出た（手元の arm64・4 スレッドで約 2.5 分）。
//
// 手順（どれも OpenMVS の CLI。作業場所は dense/openmvs/）:
//
//	MVS の段階
//	  COLMAP image_undistorter  レンズの歪みを取った画像とカメラ（dense/）
//	  InterfaceCOLMAP           COLMAP の結果を OpenMVS の形式（scene.mvs）にする
//	  DensifyPointCloud         各写真の奥行きを PatchMatch で推定し、密な点群にまとめる（scene_dense.ply）
//	メッシュの段階
//	  ReconstructMesh           点群から面を作る（ドロネー分割＋グラフカット）。面の数を OPENMVS_MAX_FACES 以下に減らす
//	  TextureMesh               各面に一番よく写っている写真を貼る（OBJ＋MTL＋JPEG で書き出す）
//	  gltfpack                  OBJ → GLB（画面と同じ形式）
//
// OpenMVS は撮影範囲（カメラの視線が集まる所）を自動で推定し、その外の点を捨てる（ROI）ので、
// 簡易メッシュのような背景の膜はほとんど出ない。

const openMVSDirName = "openmvs"

// runOpenMVSDensify は MVS の段階（歪み補正 → OpenMVS の形式へ → 密な点群）。
func (p *Pipeline) runOpenMVSDensify(ctx context.Context, job Job) (string, error) {
	ws := job.WS
	logPath := ws.LogPath(scan.StepMVS)
	if err := p.undistort(ctx, ws, logPath, p.cfg.MVSTimeout); err != nil {
		return "", err
	}
	dir := filepath.Join(ws.DenseDir(), openMVSDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	if err := p.openmvs(ctx, logPath, p.cfg.MVSTimeout, "InterfaceCOLMAP",
		"-w", dir,
		"-i", ws.DenseDir(),
		"-o", filepath.Join(dir, "scene.mvs"),
		"--image-folder", filepath.Join(ws.DenseDir(), "images")); err != nil {
		return "", err
	}
	// --resolution-level: 写真を何回半分にしてから奥行きを推定するか。1（縦横半分）で CPU でも数分に収まる。
	if err := p.openmvs(ctx, logPath, p.cfg.MVSTimeout, "DensifyPointCloud",
		"-w", dir,
		"-i", filepath.Join(dir, "scene.mvs"),
		"-o", filepath.Join(dir, "scene_dense.mvs"),
		"--resolution-level", strconv.Itoa(p.cfg.OpenMVSResolutionLevel)); err != nil {
		return "", err
	}
	cloud := filepath.Join(dir, "scene_dense.ply")
	if _, err := checkFile(cloud, 1024, []byte("ply")); err != nil {
		return "", friendly("密な点群を作れませんでした。写真の重なりや枚数が足りない可能性があります。")
	}
	n, _ := PLYVertexCount(cloud)
	return fmt.Sprintf("%d点の密な点群を作りました（OpenMVS）。", n), nil
}

// runOpenMVSMesh はメッシュの段階（面を作る → テクスチャを貼る → GLB）。
func (p *Pipeline) runOpenMVSMesh(ctx context.Context, job Job, rep Reporter) (string, error) {
	ws := job.WS
	logPath := ws.LogPath(scan.StepMesh)
	dir := filepath.Join(ws.DenseDir(), openMVSDirName)
	mesh := filepath.Join(dir, "mesh.ply")
	// --target-face-num: スマホで表示できる大きさにする（何も指定しないと数十万〜百万面になる）。
	if err := p.openmvs(ctx, logPath, p.cfg.MeshTimeout, "ReconstructMesh",
		"-w", dir,
		"-i", filepath.Join(dir, "scene_dense.mvs"),
		"-p", filepath.Join(dir, "scene_dense.ply"),
		"-o", mesh,
		"--target-face-num", strconv.Itoa(p.cfg.OpenMVSMaxFaces)); err != nil {
		return "", err
	}
	if _, err := checkFile(mesh, 200, []byte("ply")); err != nil {
		return "", friendly("点群からメッシュ（面）を作れませんでした。")
	}

	// OBJ で書き出すと、テクスチャは JPEG になる（PNG より 1/3 ほど小さい）。
	// seam leveling（継ぎ目の明るさをならす処理）は既定で切る（config.Pipeline.OpenMVSSeamLeveling の説明）。
	leveling := "0"
	if p.cfg.OpenMVSSeamLeveling {
		leveling = "1"
	}
	obj := filepath.Join(dir, "textured.obj")
	if err := p.openmvs(ctx, logPath, p.cfg.MeshTimeout, "TextureMesh",
		"-w", dir,
		"-i", filepath.Join(dir, "scene_dense.mvs"),
		"-m", mesh,
		"-o", obj,
		"--export-type", "obj",
		"--max-texture-size", strconv.Itoa(p.cfg.OpenMVSTextureSize),
		"--global-seam-leveling", leveling,
		"--local-seam-leveling", leveling); err != nil {
		return "", err
	}
	mtl := strings.TrimSuffix(obj, ".obj") + ".mtl"
	if _, err := checkFile(obj, 100, nil); err != nil {
		return "", friendly("メッシュにテクスチャを貼れませんでした。")
	}
	if err := fixOpenMVSMaterial(mtl); err != nil {
		return "", fmt.Errorf("テクスチャの材質を直せません: %w", err)
	}
	faces, err := countOBJFaces(obj)
	if err != nil {
		return "", err
	}

	glb := filepath.Join(ws.OutputDir(), "mesh.glb")
	if _, err := p.run.Run(ctx, cmdrun.Command{
		Name:    p.cfg.GltfpackBin,
		Args:    []string{"-i", obj, "-o", glb, "-noq"},
		Timeout: p.cfg.MeshTimeout,
		LogPath: logPath,
	}); err != nil {
		return "", err
	}
	if err := p.publish(ctx, rep, scan.AssetMeshGLB, glb, 1024, []byte("glTF")); err != nil {
		return "", err
	}
	return fmt.Sprintf("密な点群から %d 面のメッシュを作り、テクスチャを貼りました（OpenMVS）。", faces), nil
}

// openmvs は OpenMVS の CLI を実行する。--max-threads は NUM_THREADS に合わせる（CPU の上限を超えて走らせない）。
func (p *Pipeline) openmvs(ctx context.Context, logPath string, timeout time.Duration, tool string, args ...string) error {
	if p.cfg.NumThreads > 0 {
		args = append(args, "--max-threads", strconv.Itoa(p.cfg.NumThreads))
	}
	_, err := p.run.Run(ctx, cmdrun.Command{
		Name: filepath.Join(p.cfg.OpenMVSDir, tool), Args: args, Timeout: timeout, LogPath: logPath,
	})
	return err
}

// fixOpenMVSMaterial は TextureMesh が書く MTL を、どのソフトでも不透明に見えるよう直す。
//
// TextureMesh の MTL には "Tr 1.000000" が入る。Tr は透明度（1 = 完全に透明）と解釈するソフトがあり
// （gltfpack が読む fast_obj もその 1 つ）、そのままでは GLB の材質が透明になってしまう。Tr を消し、不透明度 d 1 を書く。
func fixOpenMVSMaterial(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && (f[0] == "Tr" || f[0] == "d") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
		if len(f) > 0 && f[0] == "newmtl" {
			out.WriteString("d 1\n")
		}
	}
	return os.WriteFile(path, bytes.TrimRight(out.Bytes(), "\n"), 0o640)
}

// countOBJFaces は OBJ の面（"f " で始まる行）の数を数える。
func countOBJFaces(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		if line := sc.Bytes(); len(line) > 1 && line[0] == 'f' && line[1] == ' ' {
			n++
		}
	}
	return n, sc.Err()
}
