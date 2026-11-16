# 03. MVS とメッシュ — 点から面を作り、写真を貼る

## このノートで分かること

- SfM の「まばらな点」から、どうやって「面のあるモデル（メッシュ）」を作るか。
- 密な復元（MVS）と、GPU が無いときの簡易メッシュの違い。なぜ 2 つの経路があるのか。
- 歪み補正・テクスチャアトラス・UV とは何か。
- COLMAP の出力（PLY）を、Web で表示できる GLB にするまでの変換の流れ。

## 目的

SfM（[02](02-sfm.md)）が出すのは、カメラの位置と、数千個の 3D 点だけ。点の集まりは形の手掛かりにはなるが、
向こう側が透けて見え、ゲームエンジンや 3D プリンタでは使いにくい。

そこで、点を三角形でつないで「面」にし（**メッシュ化**）、その面に写真を貼り付ける（**テクスチャ**）。
出来上がりは、どのソフトでも開ける GLB ファイル 1 つ。

## 入力

| 入力 | 場所 | 作ったステップ |
| --- | --- | --- |
| 写真 | `images/` | アップロード |
| カメラの位置・向き・レンズのパラメータ、疎な点群 | `sparse/0/` | SfM |

## 処理

経路は 2 つある。どちらを通るかは、COLMAP が GPU 対応かどうかで自動的に決まる
（`server/internal/reconstruction/pipeline.go` の `DenseMesh`）。

```text
                       ┌─ GPU（CUDA）あり ─→ [mvs] 歪み補正 → PatchMatch → 融合 → 密な点群
SfM の結果 ────────────┤                      [mesh] ポアソン・メッシュ化 ─┐
                       └─ GPU なし ─────────→ [mvs] 実行せず（理由を記録）   ├→ テクスチャ → OBJ → GLB
                                              [mesh] 歪み補正 → ドロネー（疎）┘
```

配備先のクラスタには GPU が無いので、**実際に動いているのは下の経路**。
上の経路は、偽の Runner を使った単体テストでコマンドの順番と引数を確認しただけで、**実機では未検証**。

### 共通: 歪み補正（image_undistorter）

```bash
colmap image_undistorter \
  --image_path images --input_path sparse/0 \
  --output_path dense --output_type COLMAP --max_image_size 1600
```

- 入力: 元の写真と、SfM が推定したレンズの歪み係数。
- 出力: `dense/images/`（歪みを取り除いた写真）、`dense/sparse/`（歪みなしのカメラに直したモデル）。

なぜ必要か。実際のレンズは、画面の端ほど直線が曲がって写る。SfM はこの歪みを係数 `k` として推定している。
以降の処理は「3D の点 (X, Y, Z) は画像の何画素目に写るか」を大量に計算するが、歪みがあると式が複雑になる。
先に**画像の方をまっすぐに直しておけば**、単純なピンホールカメラの式だけで済む:

```text
u = f · X / Z + cx      v = f · Y / Z + cy
（f: 焦点距離、(cx, cy): 画像の中心。カメラ座標で Z が奥行き）
```

この処理は CPU で動く。

### 経路 A: 密な復元（GPU あり。未検証）

```bash
colmap patch_match_stereo --workspace_path dense --workspace_format COLMAP \
  --PatchMatchStereo.geom_consistency true
colmap stereo_fusion --workspace_path dense --workspace_format COLMAP \
  --input_type geometric --output_path dense/fused.ply
colmap poisson_mesher --input_path dense/fused.ply --output_path dense/meshed.ply
```

- **PatchMatch ステレオ**: 各写真の各画素について奥行きを推定する。ある画素の奥行きを仮定すると、
  その 3D 点が隣の写真のどこに写るかが決まる。両方の周囲の模様がよく一致する奥行きほど正しい、と考えて探索する。
  画素の数だけ計算するので非常に重く、COLMAP では CUDA 専用。
- **融合**: 写真ごとの奥行きマップを 3D 空間で突き合わせ、複数の写真で整合する点だけを残して 1 つの点群にする。
- **ポアソン・メッシュ化**: 点とその向き（法線）から「内側か外側か」を表す滑らかな関数を解き、その境界を面として取り出す。

公式 FAQ には「CUDA 対応 GPU が無ければ、dense 再構成以外のすべての機能が使える」とある。
つまり、この経路は GPU が無いと実行できない。

### 経路 B: 簡易メッシュ（GPU なし。実機で確認済み）

```bash
colmap delaunay_mesher --input_path dense/sparse --input_type sparse \
  --output_path dense/meshed.ply
```

密な点群を作らず、SfM の疎な点（数千個）から直接メッシュを作る。

1. **ドロネー四面体分割**: 3D の点どうしを、細長くならないように四面体で埋め尽くす。
2. **グラフカット**: 「カメラからその点が見えた」ということは、カメラと点の間には何も無い（空間である）はず。
   この視線の情報を使って、各四面体を「物体の内側」か「外側」に分ける。
3. 内側と外側の**境界になっている三角形**を、表面として取り出す。

点が少ないので形は粗い（建物なら「正面の板」程度）。そのかわり CPU で数秒。
「形はおおまかでよいので、テクスチャ付きで全体を見たい」用途には十分で、GPU を足すまでのつなぎになる。

### 共通: テクスチャ（mesh_texturer）

```bash
colmap mesh_texturer --workspace_path dense \
  --input_path dense/meshed.ply --output_path dense/textured
```

- 入力: 歪み補正済みの写真とカメラ（`dense/`）、メッシュ。
- 出力: `dense/textured/mesh.ply` と `dense/textured/texture.png`。

各三角形について「その面を最も正面から、はっきり写している写真」を選び、その部分を切り出す。
切り出した断片を 1 枚の大きな画像に敷き詰めたものが**テクスチャアトラス**（`texture.png`）。
各三角形の 3 つの角には、アトラス上のどこを貼るかを示す座標 **(u, v)** が付く。これが UV。

```text
   メッシュの三角形               テクスチャアトラス（1 枚の画像）
        A                         (0,1)┌──────────────┐(1,1)
       / \          UV で対応      │  ▲a    ▧▧     │
      /   \     ───────────────→  │ / \    ▧▧     │
     B─────C                       │b───c          │
                                  (0,0)└──────────────┘(1,0)
   A の UV = a の位置、B = b、C = c
```

同じ頂点でも、面によって貼る場所が違う（アトラスの継ぎ目）。だから UV は「頂点ごと」ではなく
「面ごと」に 6 個の数（u0,v0, u1,v1, u2,v2）として保存されている。

**注意（ソースで確認）**: `mesh_texturer` は 4.0.0 で追加されたコマンドで、出力は OBJ ではなく
「面ごとの UV を持つ PLY」。ヘッダはこうなっている:

```text
ply
format binary_little_endian 1.0
comment TextureFile texture.png
element vertex 4586
property float x
property float y
property float z
element face 9166
property list uchar int vertex_indices
property list uchar float texcoord
end_header
```

### 共通: PLY → OBJ → GLB

gltfpack は OBJ か glTF しか読めないので、PLY を Go で OBJ + MTL に書き直す。

| ファイル | 役割 |
| --- | --- |
| `server/internal/reconstruction/ply.go` | PLY を読む（ヘッダを解釈して、頂点・面・UV を取り出す） |
| `server/internal/reconstruction/obj.go` | OBJ（形と UV）と MTL（材質。`map_Kd texture.png`）を書く |

OBJ は、頂点（`v`）と UV（`vt`）を別々の番号で持てる。だから「面ごとの UV」をそのまま表現できる:

```text
v 0 0 0            ← 頂点 1
vt 0 0             ← UV 1（1 枚目の面の 1 つ目の角）
f 1/1 2/2 3/3      ← 面。「頂点番号/UV 番号」を 3 組（番号は 1 から）
```

最後に gltfpack で GLB にする:

```bash
gltfpack -i dense/textured/mesh.obj -o output/mesh.glb -noq
```

glTF は Web 向けの標準 3D 形式で、GLB はその「1 ファイルにまとめた版」。形・材質・テクスチャ画像がすべて入る。
`-noq` は頂点の量子化（座標を整数に丸めて小さくする拡張）を使わない指定。少し大きくなるが、
拡張に対応していないソフトでも読める。

## 出力

| ファイル | 内容 | 実測（サンプル 11 枚） |
| --- | --- | --- |
| `output/mesh.glb` | テクスチャ付きメッシュ | 頂点 4,586・面 9,166、3.7MB |

所要時間は、歪み補正からGLB 化まで合わせて**約 2 秒**（簡易メッシュの経路）。

成功の判定は「コマンドが 0 で終わった」だけではない。`pipeline.go` の `publish` が、
ファイルがあること・1KB 以上であること・先頭 4 バイトが `glTF` であることを確かめてから登録する。

画面では、テクスチャの色をそのまま表示する（照明の計算をしない）。写真には撮影時の光と影が既に写り込んでいて、
さらに照明を掛けると影が二重になるため（`web/src/viewer/MeshViewer.tsx` の `unlit`）。

## よくある失敗

| 症状 | 原因 | 対処 |
| --- | --- | --- |
| 「密な復元を実行できません」と表示され、MVS が「実行せず」になる | COLMAP が CUDA 非対応（既定のイメージ） | 仕様どおり。簡易メッシュが作られる |
| メッシュが板のように薄い、穴が多い | 簡易メッシュは疎な点（数千個）から作るため | 写真を増やして点を増やす。本格的には GPU と密な復元が必要 |
| 空や地面に大きな三角形が張られる | 点の無い領域もつないでしまう | 簡易メッシュの限界。対象を画面いっぱいに撮ると減る |
| 「colmap が異常終了しました（SIGABRT）」 | 3D 点が極端に少ない。実機では 53 点の入力で発生 | 写真の重なりを増やして撮り直す |
| 面が真っ黒 | その面を正面から写した写真が無い | 足りない角度から撮り足す |
| `MESH_MODE=dense` にしたら失敗する | GPU が無いのに密な復元を強制した | `auto` に戻す |

異常終了の理由は、止まり方で区別している（`pipeline.go` の `exitHint`）。
`SIGKILL` はメモリ上限超過が典型、`SIGABRT` / `SIGSEGV` はツール自身の異常終了。
当初はどちらも「メモリ不足」と表示していたが、実機で `SIGABRT` を観測して直した。

## 確認方法

```bash
# メッシュ分岐だけを実行する（3DGS は省く）
./scripts/fetch-sample.sh
docker run --rm --entrypoint /app/reconstruct -v "$PWD/samples/sceaux:/work" \
  -e ENABLE_SPLAT=false 3d-scanner:dev -dir /work

# 途中の成果物を見る
ls samples/sceaux/dense/                 # images/ sparse/ stereo/ meshed.ply textured/
head -c 400 samples/sceaux/dense/textured/mesh.ply | head -12   # PLY のヘッダ
head -5 samples/sceaux/dense/textured/mesh.obj                  # Go が書いた OBJ
cat samples/sceaux/logs/mesh.log | grep '^\$'                   # 実行したコマンドの一覧

# 変換の単体テスト
cd server && go test ./internal/reconstruction/ -run 'PLY|OBJ' -v
```

出来た `mesh.glb` は、このアプリのビューアのほか、<https://playcanvas.com/viewer> や Blender でも開ける。

### GPU を足したとき（未検証の手順メモ）

1. ノードに NVIDIA のドライバとデバイスプラグインを入れ、`kubectl describe node` で `nvidia.com/gpu` が見えることを確認する。
2. Dockerfile の COLMAP のビルドを `-DCUDA_ENABLED=ON` にし、ベースイメージを CUDA 入りのものに替える。
   OpenSplat も `GPU_RUNTIME=CUDA` でビルドする。
3. chart で `gpu.enabled=true`、`config.pipeline.splatForceCPU=false` にする。
4. 起動ログの「復元パイプラインの構成」で `colmapGPU: true`、`meshMode: dense` になっていることを確認する。

この手順は実行していない。特に 2. はイメージが数 GB 大きくなり、ビルド手順も変わる。

## 次の処理

`mesh.glb` は成果物として登録され、ビューアの「メッシュ」タブで表示される。
メッシュ分岐が失敗しても、3DGS 分岐（[04](04-gaussian-splatting.md)）は独立に実行される。

## 次に読む

- [04-gaussian-splatting.md](04-gaussian-splatting.md) — もう一方の分岐。面を作らない表現
- [05-go-jobs.md](05-go-jobs.md) — これらのコマンドを Go からどう実行し、失敗をどう扱うか
