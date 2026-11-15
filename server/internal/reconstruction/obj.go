package reconstruction

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteOBJ はメッシュを Wavefront OBJ として書き出す（gltfpack に渡すための中間形式）。
//
// 入力:
//   - mesh: ReadPLYMesh で読んだメッシュ。
//   - objPath: 出力する .obj のパス。テクスチャ付きの場合、同じ場所に同名の .mtl も書く。
//
// 出力されるファイル（テクスチャ付きの場合）:
//
//	mesh.obj  形状と UV。先頭で "mtllib mesh.mtl" として材質ファイルを参照する。
//	mesh.mtl  材質。"map_Kd texture.png" でテクスチャ画像を参照する。
//	texture.png は mesh_texturer が既に同じディレクトリに書いているので、ここでは触らない。
//
// OBJ の書き方の要点:
//
//	v  x y z      頂点の位置
//	vt u v        テクスチャ座標（UV）。画像の左下が (0,0)、右上が (1,1)
//	f  1/1 2/2 3/3   面。「頂点番号/UV番号」を3組。番号は 1 から始まる（0 始まりではない）
//
// UV は頂点とは別の番号で持てるので、PLY の「面ごとの UV」をそのまま表現できる
// （面1つにつき vt を3行書き、f からその3行を指す）。
//
// 失敗条件: ファイルを書けない。
func WriteOBJ(mesh *Mesh, objPath string) error {
	textured := len(mesh.FaceUVs) == len(mesh.Faces) && mesh.TextureFile != ""

	f, err := os.Create(objPath)
	if err != nil {
		return err
	}
	// 数十万行を1行ずつ write システムコールで書くと遅いので、まとめて書く。
	w := bufio.NewWriterSize(f, 1<<20)

	base := strings.TrimSuffix(filepath.Base(objPath), filepath.Ext(objPath))
	if textured {
		fmt.Fprintf(w, "mtllib %s.mtl\nusemtl textured\n", base)
	}

	hasColor := len(mesh.Colors) == len(mesh.Vertices)
	for i, v := range mesh.Vertices {
		if hasColor && !textured {
			// 「v x y z r g b」は頂点色を持たせる非公式だが広く使われる拡張（色は 0〜1）。
			c := mesh.Colors[i]
			fmt.Fprintf(w, "v %g %g %g %.4f %.4f %.4f\n", v[0], v[1], v[2],
				float32(c[0])/255, float32(c[1])/255, float32(c[2])/255)
		} else {
			fmt.Fprintf(w, "v %g %g %g\n", v[0], v[1], v[2])
		}
	}

	if textured {
		for _, uv := range mesh.FaceUVs {
			fmt.Fprintf(w, "vt %g %g\nvt %g %g\nvt %g %g\n", uv[0], uv[1], uv[2], uv[3], uv[4], uv[5])
		}
	}

	for i, face := range mesh.Faces {
		if textured {
			// i 番目の面の UV は vt の (3i+1, 3i+2, 3i+3) 行目。
			t := int32(i)*3 + 1
			fmt.Fprintf(w, "f %d/%d %d/%d %d/%d\n", face[0]+1, t, face[1]+1, t+1, face[2]+1, t+2)
		} else {
			fmt.Fprintf(w, "f %d %d %d\n", face[0]+1, face[1]+1, face[2]+1)
		}
	}

	// Flush と Close のエラーも必ず見る。ディスクが一杯のとき、エラーはここで初めて現れる。
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if !textured {
		return nil
	}
	mtl := fmt.Sprintf("newmtl textured\nKa 1 1 1\nKd 1 1 1\nKs 0 0 0\nillum 1\nmap_Kd %s\n", mesh.TextureFile)
	return os.WriteFile(filepath.Join(filepath.Dir(objPath), base+".mtl"), []byte(mtl), 0o640)
}
