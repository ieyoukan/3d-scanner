package reconstruction

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// このファイルは PLY 形式のメッシュを読む最小限のパーサ。
//
// なぜ自前で読むのか:
//   COLMAP の mesh_texturer は「面ごとの UV 座標を持つ PLY（mesh.ply）＋ texture.png」を出力する。
//   一方 gltfpack が読めるのは OBJ か glTF で、PLY は読めない。そこで PLY → OBJ の橋渡しが要る。
//   PLY は「ヘッダで自分の構造を説明する」単純な形式なので、必要な部分だけなら短く書ける。
//
// PLY の構造（例: mesh_texturer の出力）:
//
//	ply
//	format binary_little_endian 1.0
//	comment TextureFile texture.png        ← 使うテクスチャ画像
//	element vertex 1234                    ← 「vertex という要素が 1234 個ある」
//	property float x                       ← 各 vertex は x, y, z の3つの float を持つ
//	property float y
//	property float z
//	element face 2345                      ← 続いて face が 2345 個
//	property list uchar int vertex_indices ← 「個数(uchar) に続いて、その個数ぶんの int」
//	property list uchar float texcoord     ← 面の3頂点の (u,v)。6個の float
//	end_header
//	（ここからヘッダに書かれた順にデータが並ぶ）

// Mesh は三角形メッシュ。
type Mesh struct {
	// Vertices は頂点の位置 (x, y, z)。
	Vertices [][3]float32
	// Colors は頂点ごとの色 (r, g, b) で各 0〜255。無ければ nil。
	Colors [][3]uint8
	// Faces は三角形。各要素は Vertices の添字3つ。
	Faces [][3]int32
	// FaceUVs は三角形ごとのテクスチャ座標 (u0,v0, u1,v1, u2,v2)。無ければ nil。
	// 頂点ごとではなく「面ごと」に持つのは、同じ頂点でも面によって貼る画像の場所が違うため
	// （テクスチャアトラスの継ぎ目）。
	FaceUVs [][6]float32
	// TextureFile はヘッダの "comment TextureFile" に書かれた画像ファイル名。無ければ空。
	TextureFile string
}

// plyProperty はヘッダの property 1行ぶん。
type plyProperty struct {
	name string
	// typ は値の型（list のときは要素の型）。
	typ string
	// countType は list の「個数」を表す型。list でなければ空。
	countType string
}

// plyElement はヘッダの element 1つぶん。
type plyElement struct {
	name  string
	count int
	props []plyProperty
}

// 読み込むメッシュの大きさの上限。壊れたヘッダ（個数が数十億など）でメモリを食い尽くさないため。
const maxPlyElements = 50_000_000

// ReadPLYMesh は PLY ファイルからメッシュを読む。
//
// 入力: PLY ファイルのパス（binary_little_endian または ascii）。
// 出力: 頂点・面・（あれば）頂点色と面ごとの UV。
// 失敗条件: PLY でない、未対応の形式（big endian）、三角形以外の面、途中で切れたファイル。
func ReadPLYMesh(path string) (*Mesh, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)

	format, elements, textureFile, err := readPLYHeader(r)
	if err != nil {
		return nil, fmt.Errorf("PLY のヘッダを読めません: %w", err)
	}

	var next func(typ string) (float64, error)
	switch format {
	case "binary_little_endian":
		next = func(typ string) (float64, error) { return readBinaryScalar(r, typ) }
	case "ascii":
		next = asciiScalarReader(r)
	default:
		return nil, fmt.Errorf("未対応の PLY 形式です: %s", format)
	}

	mesh := &Mesh{TextureFile: textureFile}
	for _, el := range elements {
		switch el.name {
		case "vertex":
			if err := readVertices(mesh, el, next); err != nil {
				return nil, fmt.Errorf("頂点を読めません: %w", err)
			}
		case "face":
			if err := readFaces(mesh, el, next); err != nil {
				return nil, fmt.Errorf("面を読めません: %w", err)
			}
		default:
			// 知らない要素（edge など）は値を読み捨てて位置だけ進める。
			if err := skipElement(el, next); err != nil {
				return nil, err
			}
		}
	}
	if len(mesh.Vertices) == 0 || len(mesh.Faces) == 0 {
		return nil, errors.New("メッシュが空です（頂点または面が0個）")
	}
	return mesh, nil
}

// readPLYHeader は "end_header" までを読み、形式と要素の定義を返す。
func readPLYHeader(r *bufio.Reader) (format string, elements []plyElement, textureFile string, err error) {
	line, err := readLine(r)
	if err != nil || line != "ply" {
		return "", nil, "", errors.New("先頭が \"ply\" ではありません")
	}
	// ヘッダは数十行のはず。終わりが見つからないファイルを延々と読まないよう上限を置く。
	for i := 0; i < 1000; i++ {
		line, err := readLine(r)
		if err != nil {
			return "", nil, "", err
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "format":
			if len(fields) < 2 {
				return "", nil, "", errors.New("format 行が不正です")
			}
			format = fields[1]
		case "comment":
			// MeshLab 由来の慣習: "comment TextureFile <ファイル名>"。
			if len(fields) >= 3 && fields[1] == "TextureFile" {
				textureFile = strings.Join(fields[2:], " ")
			}
		case "element":
			if len(fields) != 3 {
				return "", nil, "", errors.New("element 行が不正です")
			}
			n, convErr := strconv.Atoi(fields[2])
			if convErr != nil || n < 0 || n > maxPlyElements {
				return "", nil, "", fmt.Errorf("element %s の個数が不正です: %s", fields[1], fields[2])
			}
			elements = append(elements, plyElement{name: fields[1], count: n})
		case "property":
			if len(elements) == 0 {
				return "", nil, "", errors.New("element より前に property があります")
			}
			el := &elements[len(elements)-1]
			switch {
			case len(fields) == 5 && fields[1] == "list":
				el.props = append(el.props, plyProperty{name: fields[4], countType: fields[2], typ: fields[3]})
			case len(fields) == 3:
				el.props = append(el.props, plyProperty{name: fields[2], typ: fields[1]})
			default:
				return "", nil, "", fmt.Errorf("property 行が不正です: %q", line)
			}
		case "end_header":
			if format == "" {
				return "", nil, "", errors.New("format 行がありません")
			}
			return format, elements, textureFile, nil
		}
	}
	return "", nil, "", errors.New("end_header が見つかりません")
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readBinaryScalar は型名に応じたバイト数をリトルエンディアンで読み、float64 にして返す。
// 位置も色も添字も一旦 float64 で受ける（float64 は 2^53 までの整数を正確に表せるので問題ない）。
func readBinaryScalar(r io.Reader, typ string) (float64, error) {
	var buf [8]byte
	read := func(n int) error {
		_, err := io.ReadFull(r, buf[:n])
		return err
	}
	switch typ {
	case "char", "int8":
		if err := read(1); err != nil {
			return 0, err
		}
		return float64(int8(buf[0])), nil
	case "uchar", "uint8":
		if err := read(1); err != nil {
			return 0, err
		}
		return float64(buf[0]), nil
	case "short", "int16":
		if err := read(2); err != nil {
			return 0, err
		}
		return float64(int16(binary.LittleEndian.Uint16(buf[:2]))), nil
	case "ushort", "uint16":
		if err := read(2); err != nil {
			return 0, err
		}
		return float64(binary.LittleEndian.Uint16(buf[:2])), nil
	case "int", "int32":
		if err := read(4); err != nil {
			return 0, err
		}
		return float64(int32(binary.LittleEndian.Uint32(buf[:4]))), nil
	case "uint", "uint32":
		if err := read(4); err != nil {
			return 0, err
		}
		return float64(binary.LittleEndian.Uint32(buf[:4])), nil
	case "float", "float32":
		if err := read(4); err != nil {
			return 0, err
		}
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[:4]))), nil
	case "double", "float64":
		if err := read(8); err != nil {
			return 0, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(buf[:8])), nil
	}
	return 0, fmt.Errorf("未対応の型です: %s", typ)
}

// asciiScalarReader は空白区切りの数値を1つずつ返す関数を作る（ascii 形式用）。
func asciiScalarReader(r *bufio.Reader) func(string) (float64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	sc.Split(bufio.ScanWords)
	return func(string) (float64, error) {
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return 0, err
			}
			return 0, io.ErrUnexpectedEOF
		}
		return strconv.ParseFloat(sc.Text(), 64)
	}
}

// readList は list 型の property（個数＋値の並び）を読む。
func readList(p plyProperty, next func(string) (float64, error)) ([]float64, error) {
	n, err := next(p.countType)
	if err != nil {
		return nil, err
	}
	if n < 0 || n > 64 {
		return nil, fmt.Errorf("list の個数が不正です: %v", n)
	}
	vals := make([]float64, int(n))
	for i := range vals {
		if vals[i], err = next(p.typ); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

func readVertices(mesh *Mesh, el plyElement, next func(string) (float64, error)) error {
	hasColor := false
	for _, p := range el.props {
		if p.name == "red" {
			hasColor = true
		}
	}
	mesh.Vertices = make([][3]float32, 0, el.count)
	if hasColor {
		mesh.Colors = make([][3]uint8, 0, el.count)
	}
	for i := 0; i < el.count; i++ {
		var pos [3]float32
		var col [3]uint8
		for _, p := range el.props {
			if p.countType != "" {
				if _, err := readList(p, next); err != nil {
					return err
				}
				continue
			}
			v, err := next(p.typ)
			if err != nil {
				return err
			}
			// 必要な property だけ拾い、法線（nx,ny,nz）などは読み捨てる。
			switch p.name {
			case "x":
				pos[0] = float32(v)
			case "y":
				pos[1] = float32(v)
			case "z":
				pos[2] = float32(v)
			case "red":
				col[0] = clampByte(v)
			case "green":
				col[1] = clampByte(v)
			case "blue":
				col[2] = clampByte(v)
			}
		}
		mesh.Vertices = append(mesh.Vertices, pos)
		if hasColor {
			mesh.Colors = append(mesh.Colors, col)
		}
	}
	return nil
}

func readFaces(mesh *Mesh, el plyElement, next func(string) (float64, error)) error {
	mesh.Faces = make([][3]int32, 0, el.count)
	for i := 0; i < el.count; i++ {
		var face [3]int32
		var uv [6]float32
		hasFace, hasUV := false, false
		for _, p := range el.props {
			if p.countType == "" {
				if _, err := next(p.typ); err != nil {
					return err
				}
				continue
			}
			vals, err := readList(p, next)
			if err != nil {
				return err
			}
			switch p.name {
			// 名前は書き出すソフトによって vertex_indices / vertex_index の両方がある。
			case "vertex_indices", "vertex_index":
				if len(vals) != 3 {
					return fmt.Errorf("%d番目の面が三角形ではありません（頂点数 %d）", i, len(vals))
				}
				for k, v := range vals {
					if v < 0 || int(v) >= len(mesh.Vertices) {
						return fmt.Errorf("%d番目の面が存在しない頂点 %v を指しています", i, v)
					}
					face[k] = int32(v)
				}
				hasFace = true
			case "texcoord":
				if len(vals) != 6 {
					return fmt.Errorf("%d番目の面のテクスチャ座標が6個ではありません（%d個）", i, len(vals))
				}
				for k, v := range vals {
					uv[k] = float32(v)
				}
				hasUV = true
			}
		}
		if !hasFace {
			return fmt.Errorf("%d番目の面に頂点の並びがありません", i)
		}
		mesh.Faces = append(mesh.Faces, face)
		if hasUV {
			mesh.FaceUVs = append(mesh.FaceUVs, uv)
		}
	}
	// 一部の面にだけ UV がある、という中途半端な状態は扱わない。
	if len(mesh.FaceUVs) != 0 && len(mesh.FaceUVs) != len(mesh.Faces) {
		return errors.New("テクスチャ座標のある面と無い面が混ざっています")
	}
	return nil
}

func skipElement(el plyElement, next func(string) (float64, error)) error {
	for i := 0; i < el.count; i++ {
		for _, p := range el.props {
			if p.countType != "" {
				if _, err := readList(p, next); err != nil {
					return err
				}
				continue
			}
			if _, err := next(p.typ); err != nil {
				return err
			}
		}
	}
	return nil
}

func clampByte(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// PLYVertexCount は PLY のヘッダだけを読み、"element vertex N" の N を返す。
// 3DGS の PLY では「頂点」1つがガウシアン1個なので、これがガウシアンの個数になる。
func PLYVertexCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	_, elements, _, err := readPLYHeader(bufio.NewReader(f))
	if err != nil {
		return 0, err
	}
	for _, el := range elements {
		if el.name == "vertex" {
			return el.count, nil
		}
	}
	return 0, errors.New("vertex 要素がありません")
}
