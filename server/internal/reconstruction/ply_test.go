package reconstruction

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// colmapTexturedPLY は COLMAP の mesh_texturer が書くのと同じ並びのバイナリ PLY を作る
// （COLMAP 4.2.1 の src/colmap/util/ply.cc の WriteBinaryPlyMesh と同じ形式）。
func colmapTexturedPLY(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("ply\nformat binary_little_endian 1.0\ncomment TextureFile texture.png\n")
	b.WriteString("element vertex 4\nproperty float x\nproperty float y\nproperty float z\n")
	b.WriteString("element face 2\nproperty list uchar int vertex_indices\nproperty list uchar float texcoord\nend_header\n")
	le := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range [][3]float32{{0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {0, 1, 0.5}} {
		le(v)
	}
	faces := [][3]int32{{0, 1, 2}, {0, 2, 3}}
	uvs := [][6]float32{{0, 0, 0.5, 0, 0.5, 0.5}, {0.5, 0.5, 1, 1, 0.5, 1}}
	for i := range faces {
		le(uint8(3))
		le(faces[i])
		le(uint8(6))
		le(uvs[i])
	}
	return b.Bytes()
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadPLYMeshBinaryTextured(t *testing.T) {
	mesh, err := ReadPLYMesh(writeTemp(t, "mesh.ply", colmapTexturedPLY(t)))
	if err != nil {
		t.Fatal(err)
	}
	if len(mesh.Vertices) != 4 || len(mesh.Faces) != 2 || len(mesh.FaceUVs) != 2 {
		t.Fatalf("頂点%d 面%d UV%d", len(mesh.Vertices), len(mesh.Faces), len(mesh.FaceUVs))
	}
	if mesh.TextureFile != "texture.png" {
		t.Errorf("TextureFile = %q", mesh.TextureFile)
	}
	if mesh.Vertices[3] != [3]float32{0, 1, 0.5} {
		t.Errorf("4番目の頂点 = %v", mesh.Vertices[3])
	}
	if mesh.Faces[1] != [3]int32{0, 2, 3} {
		t.Errorf("2番目の面 = %v", mesh.Faces[1])
	}
	if mesh.FaceUVs[1] != [6]float32{0.5, 0.5, 1, 1, 0.5, 1} {
		t.Errorf("2番目の面の UV = %v", mesh.FaceUVs[1])
	}
	if mesh.Colors != nil {
		t.Error("色が無い PLY なのに Colors が入った")
	}
}

// TestReadPLYMeshAsciiWithColorsAndNormals: 法線など使わない property があっても正しく読み飛ばす。
// （poisson_mesher の出力は法線や色を含む。）
func TestReadPLYMeshAsciiWithColorsAndNormals(t *testing.T) {
	ply := `ply
format ascii 1.0
comment made by test
element vertex 3
property float x
property float y
property float z
property float nx
property float ny
property float nz
property uchar red
property uchar green
property uchar blue
element face 1
property list uchar int vertex_index
end_header
0 0 0 0 0 1 255 0 0
1 0 0 0 0 1 0 255 0
0 1 0 0 0 1 0 0 255
3 0 1 2
`
	mesh, err := ReadPLYMesh(writeTemp(t, "mesh.ply", []byte(ply)))
	if err != nil {
		t.Fatal(err)
	}
	if len(mesh.Vertices) != 3 || len(mesh.Faces) != 1 || mesh.FaceUVs != nil {
		t.Fatalf("頂点%d 面%d", len(mesh.Vertices), len(mesh.Faces))
	}
	if mesh.Vertices[2] != [3]float32{0, 1, 0} || mesh.Colors[1] != [3]uint8{0, 255, 0} {
		t.Errorf("頂点 = %v, 色 = %v", mesh.Vertices, mesh.Colors)
	}
}

func TestReadPLYMeshRejectsBadFiles(t *testing.T) {
	good := colmapTexturedPLY(t)
	header := "ply\nformat ascii 1.0\nelement vertex 3\nproperty float x\nproperty float y\nproperty float z\n"
	cases := map[string][]byte{
		"PLY でない":       []byte("glTF\x02\x00\x00\x00"),
		"空のファイル":        {},
		"途中で切れている":      good[:len(good)-10],
		"ヘッダだけ":         []byte("ply\nformat binary_little_endian 1.0\nelement vertex 4\nproperty float x\nend_header\n"),
		"end_header 無し": []byte("ply\nformat ascii 1.0\nelement vertex 1\nproperty float x\n"),
		"ビッグエンディアン":     []byte("ply\nformat binary_big_endian 1.0\nelement vertex 1\nproperty float x\nend_header\n\x00\x00\x00\x00"),
		"四角形の面":         []byte(header + "element face 1\nproperty list uchar int vertex_index\nend_header\n0 0 0\n1 0 0\n0 1 0\n4 0 1 2 0\n"),
		"存在しない頂点を指す面":   []byte(header + "element face 1\nproperty list uchar int vertex_index\nend_header\n0 0 0\n1 0 0\n0 1 0\n3 0 1 9\n"),
		"面が0個":          []byte(header + "element face 0\nproperty list uchar int vertex_index\nend_header\n0 0 0\n1 0 0\n0 1 0\n"),
		"個数が異常に大きい":     []byte("ply\nformat ascii 1.0\nelement vertex 99999999999\nproperty float x\nend_header\n"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadPLYMesh(writeTemp(t, "bad.ply", data)); err == nil {
				t.Fatal("不正なファイルがエラーにならなかった")
			}
		})
	}
}

func TestWriteOBJTextured(t *testing.T) {
	mesh, err := ReadPLYMesh(writeTemp(t, "mesh.ply", colmapTexturedPLY(t)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	objPath := filepath.Join(dir, "mesh.obj")
	if err := WriteOBJ(mesh, objPath); err != nil {
		t.Fatal(err)
	}
	obj, _ := os.ReadFile(objPath)
	want := `mtllib mesh.mtl
usemtl textured
v 0 0 0
v 1 0 0
v 1 1 0
v 0 1 0.5
vt 0 0
vt 0.5 0
vt 0.5 0.5
vt 0.5 0.5
vt 1 1
vt 0.5 1
f 1/1 2/2 3/3
f 1/4 3/5 4/6
`
	if string(obj) != want {
		t.Errorf("OBJ が違う:\n--- got ---\n%s--- want ---\n%s", obj, want)
	}
	mtl, err := os.ReadFile(filepath.Join(dir, "mesh.mtl"))
	if err != nil || !strings.Contains(string(mtl), "newmtl textured") || !strings.Contains(string(mtl), "map_Kd texture.png") {
		t.Errorf("MTL = %q (err=%v)", mtl, err)
	}
}

// TestWriteOBJVertexColors: テクスチャが無く頂点色がある場合は「v x y z r g b」で書き、MTL は作らない。
func TestWriteOBJVertexColors(t *testing.T) {
	mesh := &Mesh{
		Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}},
		Colors:   [][3]uint8{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}},
		Faces:    [][3]int32{{0, 1, 2}},
	}
	dir := t.TempDir()
	objPath := filepath.Join(dir, "mesh.obj")
	if err := WriteOBJ(mesh, objPath); err != nil {
		t.Fatal(err)
	}
	obj, _ := os.ReadFile(objPath)
	if !strings.Contains(string(obj), "v 0 0 0 1.0000 0.0000 0.0000\n") || !strings.Contains(string(obj), "f 1 2 3\n") {
		t.Errorf("OBJ = %s", obj)
	}
	if strings.Contains(string(obj), "mtllib") {
		t.Error("テクスチャが無いのに mtllib を書いた")
	}
	if _, err := os.Stat(filepath.Join(dir, "mesh.mtl")); !os.IsNotExist(err) {
		t.Error("テクスチャが無いのに MTL を作った")
	}
}

func TestPLYVertexCount(t *testing.T) {
	// 3DGS の PLY はヘッダの後が巨大なバイナリ。ヘッダだけ読めば個数が分かる。
	ply := "ply\nformat binary_little_endian 1.0\nelement vertex 123456\nproperty float x\nproperty float opacity\nend_header\n" + string(make([]byte, 64))
	n, err := PLYVertexCount(writeTemp(t, "splat.ply", []byte(ply)))
	if err != nil || n != 123456 {
		t.Fatalf("PLYVertexCount = %d, %v", n, err)
	}
	if _, err := PLYVertexCount(writeTemp(t, "x.ply", []byte("not ply"))); err == nil {
		t.Fatal("PLY でないのにエラーにならない")
	}
}

func TestReadBinaryScalarTypes(t *testing.T) {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, int8(-5))
	binary.Write(&b, binary.LittleEndian, uint16(65535))
	binary.Write(&b, binary.LittleEndian, int32(-70000))
	binary.Write(&b, binary.LittleEndian, float64(math.Pi))
	want := []struct {
		typ string
		v   float64
	}{{"char", -5}, {"ushort", 65535}, {"int", -70000}, {"double", math.Pi}}
	for _, w := range want {
		got, err := readBinaryScalar(&b, w.typ)
		if err != nil || got != w.v {
			t.Errorf("%s = %v (err=%v), want %v", w.typ, got, err, w.v)
		}
	}
	if _, err := readBinaryScalar(&b, "quaternion"); err == nil {
		t.Error("知らない型がエラーにならない")
	}
}
