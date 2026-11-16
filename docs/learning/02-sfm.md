# 02. SfM — 写真からカメラの位置と疎な点群を求める

## このノートで分かること

- SfM（Structure from Motion）が何を入力に、何を出力するのか
- COLMAP の 3 つのコマンド（特徴抽出・マッチング・マッピング）がそれぞれ計算していること
- COLMAP の座標系と、`images.txt` からカメラの位置を取り出す式
- 失敗したときにログのどこを見ればよいか（実際に観測した失敗例つき）

## 目的

写真だけを手掛かりに、次の2つを同時に求める。

1. 各写真を撮った**カメラの位置・向き・レンズの特性**
2. 写真に写っている点の **3D 座標**（まばらなので「疎な点群」と呼ぶ）

これが以降のすべての土台になる。メッシュ分岐も 3DGS 分岐も SfM の結果を読む。SfM が失敗したら全体が失敗する。

## 入力

`/data/scans/<id>/images/` の写真（JPEG / PNG）。カメラの位置情報は一切要らない。
必要なのは「隣り合う写真に同じ点が十分に写っていること」だけ。

## 処理

実装は `runSfM`（`server/internal/reconstruction/pipeline.go`）。COLMAP 4.2.1 の CLI を順に呼ぶ。

```text
images/ ─▶ feature_extractor ─▶ exhaustive_matcher ─▶ mapper ─▶ sparse/0/
              │ 特徴点             │ 対応                │ カメラ＋3D点
              └────── database.db（SQLite）に保存 ───────┘
```

### (1) 特徴抽出 — 見つけやすい点を取り出す

```bash
colmap feature_extractor \
  --database_path /data/scans/<id>/database.db \
  --image_path    /data/scans/<id>/images \
  --ImageReader.camera_model SIMPLE_RADIAL \
  --ImageReader.single_camera 1 \
  --FeatureExtraction.use_gpu 0 \
  --FeatureExtraction.max_image_size 1600
```

各写真から「角や模様など、別の写真でも同じ場所だと分かる点」（SIFT 特徴点）を取り出し、
点ごとに周囲の模様を表す 128 次元のベクトル（記述子）を付ける。

- `camera_model SIMPLE_RADIAL`: レンズを 4 つの数（焦点距離 f、中心 cx, cy、歪み係数 k）で表す。
  OpenSplat が対応しているモデルの1つでもある（→ 04）。
- `single_camera`: 全写真の寸法が同じなら 1（同じカメラ・同じズームとみなして f, cx, cy, k を共有）。
  寸法が混ざっていたら 0。判定は `inspectImages`（`server/internal/reconstruction/images.go`）。
  未知数が減るぶん推定が安定する。
- `use_gpu 0`: GPU 版の SIFT は画面（OpenGL）を必要とする。COLMAP のチュートリアルにも
  「サーバーでは CPU 版を推奨」とある。
- オプション名は COLMAP 4 系のもの。3 系（Ubuntu 24.04 の apt 版 3.9.1 など）は
  `--SiftExtraction.use_gpu` で、名前が違う。

### (2) マッチング — 写真どうしで同じ点を探す

```bash
colmap exhaustive_matcher --database_path .../database.db --FeatureMatching.use_gpu 0
```

写真の全ペア（N 枚なら N(N−1)/2 組）について、記述子が似ている特徴点を対応づける。
対応は「2 台のカメラの位置関係が満たすべき式」（エピポーラ幾何）で検証され、誤対応が除かれる。
総当たりは数十枚なら確実で速い。数百枚を超えるなら `sequential_matcher` などに替える。

### (3) マッピング — カメラと 3D 点を同時に解く

```bash
colmap mapper --database_path .../database.db --image_path .../images --output_path .../sparse
```

インクリメンタル SfM と呼ばれる手順:

```text
1. 対応の多い 2 枚を選び、相対的な位置関係を求める
2. 三角測量: 2 台のカメラから同じ点への視線が交わる場所を、その点の 3D 座標とする
3. 既にある 3D 点が写っている次の 1 枚を選び、そのカメラの位置を求める（PnP）
4. 新しい 3D 点を三角測量で足す
5. バンドル調整: 全カメラと全 3D 点をまとめて微調整する
6. 3〜5 を、足せる写真が無くなるまで繰り返す
```

バンドル調整が最小にするのは**再投影誤差**の総和。3D 点 `X_j` をカメラ `i` に投影した位置と、
写真で実際に見つかった位置 `x_ij` のずれ:

```text
最小化:  Σ_ij ‖ π(R_i · X_j + t_i) − x_ij ‖²
         π は「カメラ座標 → 画素」の投影（焦点距離とレンズの歪みを含む）
```

この値が小さいほど、カメラと点の推定が写真と矛盾していない。サンプルでは平均 0.63 画素だった。

### (4) 結果の検証と要約

コマンドが 0 で終わっただけでは成功にしない。`selectBestModel`（同ファイル）が

- `sparse/` の各モデル（`0`, `1`, …）をテキストに変換して読み、登録画像が最も多いものを `sparse/0` に置く
  （写真が 1 つながりにならないと、mapper は複数のモデルを出力する）
- 登録できた写真が 3 枚未満（`minRegisteredImages`）なら失敗にする

続いて `model_converter --output_type PLY` で点群を `output/sparse.ply` に、
`WriteSceneInfo` でビューア用の要約を `output/scene.json` に書く。

## COLMAP の座標系

```text
世界座標   SfM が勝手に決めた座標系。原点・向き・縮尺に物理的な意味は無い
           （写真だけからは「1 が何メートルか」も「どちらが上か」も決まらない）

カメラ座標        Z（奥。レンズの向く方向）
                 ↗
        カメラ ●────▶ X（右）
               │
               ▼ Y（下）
```

`images.txt` の 1 行は「世界 → カメラ」の変換を持つ。

```text
IMAGE_ID QW QX QY QZ TX TY TZ CAMERA_ID NAME        ← 四元数 (QW..QZ) が回転 R、(TX,TY,TZ) が t
X_cam = R · X_world + t
```

ここから導けるもの（`poseFromQuaternion`、`server/internal/reconstruction/sceneinfo.go`）:

| 知りたいもの | 式 | 理由 |
| --- | --- | --- |
| カメラの位置（世界座標） | `C = −Rᵀ t` | カメラの原点は `X_cam = 0`。`0 = R·C + t` を解く |
| カメラの「下」（世界座標） | `R` の 2 行目 | `Rᵀ (0,1,0)ᵀ` |
| カメラの「奥」（世界座標） | `R` の 3 行目 | `Rᵀ (0,0,1)ᵀ` |

「t がカメラの位置」ではない点に注意（よくある取り違え）。

### scene.json — ビューアのための要約

`ReadSceneInfo` が `images.txt` と `points3D.txt` から次を計算する。

| 項目 | 求め方 | ビューアでの使い道 |
| --- | --- | --- |
| `up` | 全カメラの「下」の平均の逆向き | モデルを立てる（人はスマホをほぼ水平に持つので重力の逆に近い） |
| `center` | 点群の座標ごとの中央値 | 回転の中心。平均でないのは外れ値（遠くの背景）に引っ張られないため |
| `radius` | 中心からの距離の 75 パーセンタイル | ズームの範囲と描画範囲 |
| `cameraPositions` | 各カメラの `C` | 初期視点（1 枚目を撮った位置から見る） |

サンプルの実測値: `up ≈ (0.022, −0.999, 0.029)`、`center ≈ (−2.16, 0.56, 10.32)`、`radius ≈ 3.92`。
`up` がほぼ `−Y` なのは、COLMAP のカメラ座標の Y が下向きで、世界座標が最初のカメラにほぼ揃うため。
画面側は `computeViewFrame`（`web/src/viewer/sceneFrame.ts`）がこれを回転と平行移動に直し、
メッシュ・3DGS・点群の 3 つのビューアで共有する。ファイル自体の座標は書き換えない。

## 出力

```text
/data/scans/<id>/
  database.db          特徴点とマッチ（SQLite。アプリの PostgreSQL とは無関係）
  sparse/0/            cameras.bin, images.bin, points3D.bin（公式チュートリアルによれば 4 系は rigs.bin, frames.bin も）
  sparse_txt/0/        テキスト版（images.txt, points3D.txt など。人が開いて読める）
  output/sparse.ply    色つきの点群（ビューアの「点群」タブ）
  output/scene.json    上方向・中心・カメラ位置
  logs/sfm.log         実行した全コマンドとその出力
```

実測（サンプル 11 枚、CPU のみ、Apple Silicon 8 コアの Docker）:

| 入力 | 登録 | 3D 点 | 再投影誤差 | 時間 |
| --- | --- | --- | --- | --- |
| 元の 2832×2128 を 11 枚（curl で直接） | 11 / 11 | 8,348 | 0.63 px | 1 分 2 秒 |
| 画面経由で長辺 2048 に縮小した 10 枚 | 10 / 10 | 8,195 | 0.47 px | 34 秒 |

## よくある失敗

実際に観測したもの:

| 入力 | ログ（`logs/sfm.log`） | 結果 |
| --- | --- | --- |
| 単色の画像 8 枚 | `Features: 0 (SIFT)` → `No images with matches` → `Failed to create any sparse model`、mapper が終了コード 1 | `failed`。画面には「写真どうしの対応が見つからず…」と撮り直しの助言を出す（`noSparseModel` で判定） |
| 規則的な模様の画像 8 枚 | 4 枚だけ登録、3D 点 53 個 | SfM は成功扱い。ただし点が少なすぎて後段のメッシュ化が異常終了した（→ 03） |

原因と対処の一般論:

| 症状 | 主な原因 | 対処 |
| --- | --- | --- |
| モデルが 1 つも出来ない | 重なり不足、無地・光沢・透明な被写体、ぶれ | 7 割重ねて撮り直す |
| 一部の写真しか登録されない | その写真だけ重なりが少ない、ぶれている | 間を埋める写真を足す。画面には「N枚中M枚」と出る |
| モデルが複数に分かれる | 撮影が途中で途切れている | 最大のものだけを使う（`selectBestModel`）。残りは `sparse/smaller_0` などに残る |
| 形が歪む | カメラの位置が動いていない（その場で向きだけ変えた） | 位置を移動しながら撮る。視差が無いと奥行きは決まらない |

2 つ目の例のように「SfM は通ったが点が極端に少ない」場合を SfM の時点で失敗にする閾値は、今は設けていない。

## 確認方法

```bash
# パイプラインだけを DB も HTTP も無しで実行する（結果は samples/sceaux/output と logs に残る）
./scripts/fetch-sample.sh
docker run --rm --entrypoint /app/reconstruct -v "$PWD/samples/sceaux:/work" \
  -e ENABLE_MESH=false -e ENABLE_SPLAT=false 3d-scanner:dev -dir /work

# カメラの姿勢を自分の目で読む（1 画像につき 2 行。2 行目は特徴点の一覧）
head -8 samples/sceaux/sparse_txt/0/images.txt

# 計算の単体テスト（四元数 → 位置、要約）
cd server && go test ./internal/reconstruction/ -run 'PoseFromQuaternion|SceneInfo' -v
```

上の `docker run` は最終イメージで実行して確認した（SfM は 35 秒）。メッシュと 3DGS の両方を無効にしているので、
最後に「全体の状態: failed（3Dデータを1つも作れませんでした。）」と表示され、終了コードは 1 になる。
これは「見られる 3D データが 1 つも無ければ失敗」という規則どおりの結果で、SfM の成果物
（`output/sparse.ply`、`output/scene.json`、`sparse_txt/0/`）は残っている。
イメージは UID 10001 で動くので、Linux ではマウントするディレクトリをその UID が書けるようにしておく。

画面では、処理状況の「カメラ位置の推定（SfM）」に登録枚数・点数・再投影誤差が出る。
ビューアの「点群」タブでは、点群と（琥珀色の点で）カメラ位置を確かめられる。

## 次の処理

`sparse/0` を 2 つの分岐が読む。互いに独立で、片方が失敗してももう片方は実行される。

## 次に読む

- [03-mvs.md](03-mvs.md) — メッシュ分岐
- [04-gaussian-splatting.md](04-gaussian-splatting.md) — 3DGS 分岐
