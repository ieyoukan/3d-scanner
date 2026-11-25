# 3D Scanner

スマホのブラウザで写真を撮り、サーバーで 3D モデルを作って、ブラウザで見るための Web アプリ（MVP）。

```text
撮影（ブラウザ） → アップロード → SfM → メッシュ（GLB）
                                      └→ 3D Gaussian Splatting（PLY / SOG）
                                 → 進行状況 → 3D ビューア → ダウンロード
```

研究や学習で「まず動かして、育てていく」ための土台として作ってある。コードのコメントと文書は日本語で、
**なぜそうしているか**を書くようにしている。

- 設計の全体像: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- 調べたことと採用した点: [docs/research.md](docs/research.md)
- 学習ノート: [docs/learning/](docs/learning/)（撮影 → SfM → MVS → 3DGS → Go のジョブ → Helm の順）

## 何が出来るか

| 画面 | 内容 |
| --- | --- |
| `/` | 動画で撮る（録画ボタンを押して対象のまわりを1周。撮りながら1秒に3枚、ぶれの少ないコマを写真にする）、撮ってある動画を選ぶ、写真で撮る、または写真を選ぶ。サムネイルで確認して不要な写真を削除し、送信する。自動撮影（止まった瞬間に、景色が変わっていれば撮る）と静止画での撮影を選べる |
| `/scans/<id>` | 処理の進行状況。段階ごとの状態・説明・所要時間・処理ログ。中止と削除 |
| `/scans/<id>/view` | メッシュ / 3DGS / 点群をタブで切り替えて 3D 表示。GLB / PLY / SOG のダウンロード。名前の変更。Android の Chrome では「AR で見る」でメッシュを机や床に置いて見られる（AR で撮ったものは実物大） |
| `/gallery` | これまでに作った 3D データの一覧（サムネイル・名前・形式）。処理中や失敗したものも出る |
| `/scans/<id>/resume` | 続きを撮る。現地で撮った写真を保存済みの 3D データに位置合わせ（VPS）し、地図に前回の撮影位置と今回の位置・向きを出す。撮り足した写真を同じ座標に合わせて 3D データを作り直す |

出来ないこと（範囲外）: アカウント、3DGS の編集、撮影中に 3D データを作り足していくこと。
完成した 3D データ（成功・一部成功）はギャラリーに保存され、利用者が削除するまで残る。
失敗・中止したものは保存期間（既定 24 時間）を過ぎると自動で削除される。

### 3DGS は必要なときだけ作る

CPU での 3DGS の学習は数十分かかるので、既定では自動で作らない（`SPLAT_AUTO=false`）。
メッシュが出来た時点でスキャンを完成にし（すぐ見られる）、3DGS が欲しいものだけ、処理状況の画面か
ビューアの「3DGS を作る」を押す。押すと別の待ち行列に入れて、手が空いたときに学習する。
`SPLAT_AUTO=true` にすると、完成したスキャンは全部、自動で待ち行列に入る（`SPLAT_DEFERRED=true` の場合）。
学習中に新しいスキャンが来たら、学習を止めて新しいスキャンの位置推定とメッシュを先に済ませ、学習は後で最初からやり直す
（`SPLAT_YIELD=true`。GPU で学習するなら `false` にすると並行して動く）。
保存済みのスキャンの 3DGS は、処理状況の画面の「3DGS を作り直す」でいつでも作り直せる（GPU を使えるようになった後など）。

### 続きを撮る（VPS）

保存した 3D データの SfM の結果（`database.db` の特徴点と `sparse/0` のカメラ位置・3D 点）を「地図」として使う。

- **位置合わせ**: 写真 1 枚の特徴点を地図の写真と突き合わせ、2D-3D の対応からカメラの位置と向きを解く
  （COLMAP の `image_registrator`。PnP を RANSAC で頑健に解く）。1 枚 10〜20 秒ほど。
- **作り直し**: 前の版の写真と地図を引き継いだ新しい版を作る。COLMAP は引き継いだ写真の特徴点とマッチを
  飛ばし、今回の写真だけを処理する。前の版のカメラ位置は固定する（`--Mapper.fix_existing_frames 1`）ので、
  座標系は版をまたいで変わらない。
- 前の版は書き換えない。新しい版が完成すると、ギャラリーには新しい版だけが出る（前の版は新しい版の画面から辿れる）。
- 写真の上限（`MAX_IMAGES`）は、引き継いだ写真と合わせた枚数に掛かる（処理は全部の写真で行うため）。
- **AR で撮り足す**（Android の Chrome）: 「AR」で始めると、まず前回撮った所を映してもらい、カメラ画像を自動で位置合わせに送る。
  重なったら、前回の被写体の中心にドームを置き（タップ不要）、前回撮った方向を最初から緑にし、前回のメッシュを半透明で重ねる
  （右上の目のボタンで隠せる）。あとは琥珀色の方向へ回って撮り足す。前回を AR で撮っていれば（実際の大きさが分かっていれば）
  位置合わせ 1 回で重なる。分からなければ「横へ 30cm ほど動く」と出て、離れたもう 1 か所でも合わせて大きさを求める。

### 風景モード（部屋・庭・街並み）

撮影画面の上の切り替えを「風景」にすると、物を囲まずに、歩きながら空間を撮れる。

- **撮り方**: 撮りたい物から目を離さずに、横へ歩く。その場で向きだけ変えると奥行きが分からない（パノラマと同じ）。
  遠くの山や空、水面、動く人や車は立体にならない。
- **AR で撮る**: ドームの代わりに、歩いた道を真上から見た小さな図（撮った所が緑の点と向き）を出す。写真は時間ではなく
  動いた量（15cm か 12°）ごとに選ぶので、立ち止まっても同じ写真が溜まらない。その場で向きだけ変えている
  （1.5 秒で 35° 以上回ったのに 12cm も動いていない）と「横に歩く」、速すぎる（秒速 0.8m か 60° 以上）と「ゆっくり」と出る。
- **枚数**: `MAX_SCENE_IMAGES`（既定 150 枚）まで。写真は長辺 1600px にして送る（150 枚でも 1 回の送信に収まる）。
- **SfM**: 全部の組を照合すると枚数の二乗で遅くなるので、撮った順で前後 8 枚、16・32・64・128 枚離れた写真（1 周して
  戻ってきた所）、AR の位置が近い 5 枚（同じ所を 2 回通った所）の組だけを照合する。
- **失敗したとき**: 処理状況の SfM の説明に、位置を推定できなかった写真を「41〜55枚目」のように出す。その辺りで速く動いた・
  その場で回った・白い壁を写した可能性がある。写真が何か所かに分かれてつながったときは、その数も出す。
- 風景は 3DGS の方がきれいに見える（「3DGS を作る」）。

### 実際の大きさ（AR で撮ったとき）

写真だけの SfM は「1 が何メートルか」「どちらが上か」が分からない。AR で撮った写真には、撮った瞬間のカメラの位置
（ARCore が測ったメートル単位、y が上）を付けて送る。サーバーは SfM の後で、同じ写真の SfM でのカメラ位置と
突き合わせ（Horn の方法で縮尺・回転・平行移動を解く）、`scene.json` に `metersPerUnit`（1 が何メートルか）を書き、
`up` を AR が測った重力の向きにする。「AR で見る」はこれで実物大になる。
5 枚以上・カメラが 5cm 以上動いた・合わせた後のずれが小さいときだけ使う（処理状況の SfM の説明に「実際の大きさが分かりました」と出る）。

### GPU が無い環境での動作

既定のコンテナイメージは **CPU 専用**。配備先に NVIDIA の GPU が無いことを前提にしている。

| 出力 | GPU なし（既定） | NVIDIA GPU あり（別イメージが必要。未検証） |
| --- | --- | --- |
| メッシュ | **OpenMVS** で密な復元（CPU。31 枚で数分） | COLMAP の密な復元（MVS）によるメッシュ |
| 3DGS | CPU で学習（1 反復あたり約 1 秒。遅い） | GPU で学習 |

COLMAP の密な復元（`patch_match_stereo`）は CUDA が必須で、CPU では実行できない。そこで GPU が無いときは
OpenMVS（CPU で動く）で密な点群 → メッシュ → テクスチャを作る（`MESH_MODE=auto` の既定）。1 周の動画（31 枚）で、
疎な点群からの簡易メッシュ（9,507 面。細い棒が消え、白い天板が平らになり、背景の膜が広がる）に比べ、
約 15 万面で棒と上の看板まで出た。OpenMVS も無い環境では、従来どおり簡易メッシュを作る。

## 必要なもの

| 用途 | 必要なもの |
| --- | --- |
| 全体をコンテナで動かす | Docker（Compose v2）。ディスクの空き 10GB 以上、メモリ 8GB 以上 |
| サーバーの開発 | Go 1.27 以上 |
| Web の開発 | Node.js 22 以上 |
| 配備 | Helm 3 以上、Kubernetes |

COLMAP などの外部ツールはコンテナイメージに入っているので、手元に入れる必要は無い。

## いちばん簡単な動かし方（Docker Compose）

```bash
./scripts/dev-env.sh          # .env を作る（DB パスワードを乱数で生成）
docker compose up --build     # 初回は worker の COLMAP・OpenMVS・OpenSplat のビルドに 1 時間ほどかかる
```

本番と同じく、web（nginx）・api・worker・PostgreSQL の 4 つのコンテナで動く。手元では web の nginx が `/api` を
api に転送する（本番では Ingress が振り分ける）。

`http://localhost:8080` を開く（アプリ自体にログインは無い。ポートは手元の 127.0.0.1 にだけ公開される）。
8080 番を他のプログラムが使っている場合は、`.env` に `APP_PORT=18080` のように書く。

サンプルの写真で試す:

```bash
./scripts/fetch-sample.sh     # samples/sceaux/images/ に 11 枚を取得（リポジトリには含めていない）
```

画面の「追加」から、取得した写真を選ぶ。

> スマホ実機のカメラで試すには **HTTPS** が必要（ブラウザは HTTP のページにカメラを使わせない）。
> `http://localhost` は例外的に許可されるが、スマホから PC の IP アドレスへ HTTP で繋ぐ場合は許可されない。
> Cloudflare Tunnel などで HTTPS の URL を用意する。HTTP のままでも「写真を選ぶ」は使える。

## 開発

### サーバー（Go）

```bash
cd server
go vet ./...
go test ./...                 # PostgreSQL を使うテストは省略される

# PostgreSQL を使う契約テストも流す場合（テストは scans テーブルを空にするので専用の DB を使う）
TEST_DATABASE_URL='postgres://user:pass@localhost:5432/scanner_test?sslmode=disable' go test ./internal/store/
```

画面だけを開発したいとき（COLMAP を入れていない PC）は、`REQUIRE_TOOLS=false` を付けると起動できる。
この場合、復元ジョブは SfM で失敗する。

```bash
DATABASE_URL='postgres://...' \
REQUIRE_TOOLS=false DATA_DIR=./data go run ./cmd/server
```

### Web（Vite + React）

```bash
cd web
npm ci
npm run dev        # http://localhost:5173 。/api は http://localhost:8080 に中継される
npm run typecheck
npm test
npm run build      # dist/ に出力
```

中継先は `web/.env` の `VITE_API_TARGET` で変えられる。

### パイプラインだけを実行する

DB も HTTP も無しで、サーバーと同じ復元処理を 1 回だけ実行できる。外部 CLI をどの順で、
どんな引数で呼んでいるかを確かめるのに使う。

```bash
./scripts/fetch-sample.sh
docker run --rm -v "$PWD/samples/sceaux:/work" -e SPLAT_ITERATIONS=300 \
  --entrypoint /app/reconstruct 3d-scanner:dev -dir /work
# 結果: samples/sceaux/output/  ログ: samples/sceaux/logs/
```

## 設定（環境変数）

| 変数 | 既定値 | 内容 |
| --- | --- | --- |
| `ROLE` | `all` | 役割。`api`（受付・配信。重い処理は worker に頼む）/ `worker`（復元・3DGS・位置合わせ。内部用 HTTP を待ち受ける）/ `all`（1 プロセスで両方。開発用） |
| `WORKER_URL` | – | `ROLE=api` のとき、worker の内部用 HTTP（例 `http://scanner-worker:8081`） |
| `DATABASE_URL` | – | PostgreSQL の接続文字列。または `DB_HOST` `DB_PORT` `DB_USER` `DB_PASSWORD` `DB_NAME` を個別に指定 |
| `DATA_DIR` | `/data` | 作業領域（PVC のマウント先）。api と worker で同じ場所にする |
| `WEB_DIR` | – | ビルド済み Web の場所（`ROLE=all` で画面も配るとき）。本番は web の Pod が配る |
| `RETENTION` | `24h` | 保存期間（ギャラリーに保存したものには効かない） |
| `KEEP_COMPLETED` | `true` | 完成したスキャン（成功・一部成功）をギャラリーに保存し、期限で消さない |
| `MIN_IMAGES` / `MAX_IMAGES` | `8` / `80` | 写真の枚数 |
| `MAX_SCENE_IMAGES` | `150` | 風景モード（歩きながら空間を撮る）の写真の上限 |
| `MAX_IMAGE_MB` / `MAX_UPLOAD_MB` | `15` / `95` | 1 枚と全体のサイズ上限（Cloudflare の 100MB 制限より小さくする） |
| `MAX_QUEUED_SCANS` | `5` | 順番待ち＋実行中の上限 |
| `MIN_FREE_DISK_MB` | `2048` | 空きがこれを下回ったら受付を断る |
| `ENABLE_MESH` / `ENABLE_SPLAT` | `true` | 各分岐を実行するか |
| `MESH_MODE` | `auto` | `auto`（GPU があれば `dense`、無く OpenMVS があれば `openmvs`、どちらも無ければ `sparse`）/ `openmvs` / `dense` / `sparse` |
| `OPENMVS_DIR` | – | OpenMVS の実行ファイルの場所（worker イメージでは `/usr/local/bin/OpenMVS`） |
| `OPENMVS_RESOLUTION_LEVEL` | `1` | 密な復元で写真を何回半分にするか（0 は等倍で数倍遅い） |
| `OPENMVS_MAX_FACES` | `150000` | メッシュの面の数の上限 |
| `OPENMVS_TEXTURE_SIZE` | `4096` | テクスチャ画像の 1 辺の上限 |
| `OPENMVS_SEAM_LEVELING` | `false` | テクスチャの継ぎ目の明るさをならす（手元の arm64 ビルドで壊れたので既定は切る） |
| `COLMAP_MAX_IMAGE_SIZE` | `1600` | 特徴抽出・歪み補正時の長辺の上限 |
| `NUM_THREADS` | `0` | 外部 CLI のスレッド数（0 は全コア）。CPU 制限と同じ値にする |
| `SPLAT_ITERATIONS` | `1000` | 3DGS の学習反復回数（CPU では 1 反復あたり約 1 秒） |
| `SPLAT_DOWNSCALE` | `2` | 学習時の画像の縮小率 |
| `SPLAT_FORCE_CPU` | `true` | 3DGS を CPU で学習する |
| `SPLAT_DEFERRED` | `true` | 3DGS をメッシュの後に続けて作らず、待ち行列に入れてあとで作る |
| `SPLAT_AUTO` | `false` | 3DGS を自動で作る。`false` なら「3DGS を作る」を押したものだけ作る |
| `SPLAT_YIELD` | `true` | あとで作る 3DGS の学習中に新しいスキャンが来たら、止めて順番を譲る（GPU なら `false` で並行） |
| `SFM_TIMEOUT` ほか | `1h` ほか | 各ステップの制限時間（`MVS_` `MESH_` `SPLAT_` `SOG_`） |
| `REQUIRE_TOOLS` | `true` | 起動時に COLMAP を確認できなければ起動を中止する |

全項目と説明は `server/internal/config/config.go` にある。

## API の例

```bash
BASE=http://localhost:8080

# 写真を送って復元ジョブを登録する（すぐに 202 が返る）
curl -X POST "$BASE/api/scans" \
  -F images=@photo1.jpg -F images=@photo2.jpg -F images=@photo3.jpg  # 実際は 8 枚以上
# → {"id":"38b5d76c-...","status":"queued","frameCount":11,"duplicatesSkipped":0,"expiresAt":"..."}

# 状態を確認する（終わるまで数秒おきに繰り返す）
curl "$BASE/api/scans/$ID"

# → {"status":"running","steps":[{"name":"sfm","status":"succeeded","message":"11枚中11枚の..."}, ...],"assets":[...]}

# ギャラリーの一覧（新しい順。続きは ?after=<nextCursor>）と、名前の変更
curl "$BASE/api/scans?limit=24"
curl -X PATCH "$BASE/api/scans/$ID" -H 'Content-Type: application/json' -d '{"title":"机の上のマグカップ"}'

# 位置合わせ（VPS）: 写真 1 枚が、完成したスキャンの地図のどこから撮ったものか
curl -X POST "$BASE/api/scans/$ID/localize" -F image=@now.jpg
# → {"localized":true,"position":[7.62,0.47,1.31],"forward":[-0.22,-0.02,0.97],"matches":1492,...}

# 3DGS を作り直す（待ち行列に入れる。前の 3DGS は出来上がるまで見られる）
curl -X POST "$BASE/api/scans/$ID/splat"

# 続きを撮る: 写真を足した新しい版を登録する（1 枚から。すぐに 202 が返る）
curl -X POST "$BASE/api/scans/$ID/resume" -F images=@more1.jpg -F images=@more2.jpg
# → {"id":"0382b41f-...","status":"queued","frameCount":11,"baseFrameCount":7,...}

# 処理ログ
curl "$BASE/api/scans/$ID/logs?step=sfm"

# 成果物（mesh_glb / gaussian_sog / gaussian_ply / sparse_ply / scene_json）
curl -o mesh.glb "$BASE/api/scans/$ID/assets/mesh_glb"

# 中止（処理中）または削除（終了済み）
curl -X DELETE "$BASE/api/scans/$ID"
```

エラーは `{"error":{"code":"too_few_images","message":"写真が足りません…"}}` の形で返る。

## 写真の撮り方

**AR で撮る（Android の Chrome、ARCore 対応機種）。** 撮影画面で「AR」を選んで始め、端末をゆっくり動かすと輪（照準）が出る。
対象に合わせてタップすると、対象のまわりに半球（ドーム）が出る。輪は床や机の面・対象の表面に付き、白い机などで何も
見つからないときは前方の空中に浮かんで出る（撮りながら、視線の集まる所へ中心が自動で寄っていく）。録画ボタンを押して対象を向いたまわりを歩くと、撮れた方向のタイルが緑になり、
次に撮る方向が琥珀色に光り、画面の端の矢印が左右・上を指す。上の小さな図は真上から見たドームで、自分のいる方向がいつも下に来る。
横の1周が埋まると停止ボタンが緑になる。写真の選び方は「動画で撮る」と同じ。iPhone の Safari は WebXR の AR に対応していないので出ない。

**いちばん簡単なのは動画。** 撮影画面を「動画」にして録画ボタンを押し、対象のまわりを歩きながら 10〜30 秒かけて
1 周したら止める。撮りながら 1/3 秒ごとの区間で一番くっきりしたコマを写真にしていく（録画はしない。動画として
圧縮する前のカメラの映像から取る）。止まって撮る必要はない。長く撮って上限（80 枚）に達したら、隣り合う 2 枚の
くっきりした方を残して半分にし、以後は間隔を倍にする。カメラアプリで撮ってある動画は「動画を選ぶ」で使える
（ブラウザが動画を再生しながら、同じ選び方で写真にする。動画そのものは送らない）。

写真で撮るときは:

1. **対象のまわりを、少しずつ移動しながら撮る。** 同じ場所で向きだけ変えても奥行きは分からない。
2. **隣の写真と 7 割ほど重ねる。** 写真どうしで同じ点を見つけて位置を合わせるので、重なりが要る。
3. **高さを変えて、もう 1 周。** 上からと下からの写真があると、隠れた面まで復元できる。
4. **最低 8 枚、できれば 20〜60 枚。**
5. **光る面・透明な物・無地の壁は苦手。** 見る角度で見た目が変わる面や、模様の無い面では同じ点を見つけられない。
6. **止まってから撮る。** ぶれた写真は位置合わせに使えない。確認画面で「ぼけ?」と出た写真は削除する。
7. **自動撮影を使う。** 映像の下の「移動 → 止める」のバーに従う。横へ移動するとバーが伸び、
   いっぱいになって黄色に変わったら止める。止まると撮る。手が震えて止まりきれなくても、1 秒ほど待てば撮る。
   重なりの足りない写真やぶれた写真が減る。
8. **スマホのカメラアプリで撮った写真を選んでもよい。** 写真に記録された焦点距離（35mm 換算）を送る前に読み取り、
   位置推定の初期値に使う（縮小し直すと EXIF は消えるため、別に送っている）。

詳しくは [docs/learning/01-capture.md](docs/learning/01-capture.md)。

## コンテナイメージ

役割ごとに 3 つのイメージを作る（`--target` で選ぶ）。

```bash
docker build -f deploy/docker/Dockerfile --target web    -t 3d-scanner-web:dev .
docker build -f deploy/docker/Dockerfile --target api    -t 3d-scanner-api:dev .
docker build -f deploy/docker/Dockerfile --target worker -t 3d-scanner-worker:dev .
```

| イメージ | 土台 | 中身 |
| --- | --- | --- |
| web | nginx（非 root、Alpine） | ビルド済みの画面 |
| api | distroless（Debian 13。シェルもパッケージ管理も無い） | Go の単一バイナリ |
| worker | Debian 13（slim） | Go のバイナリと、CPU 専用でビルドした COLMAP 4.2.1・OpenMVS 2.4.0・OpenSplat 1.2.2・gltfpack 1.3・splat-transform 3.11.2 |

worker の実行に要る共有ライブラリは、ビルドした実行ファイルから `ldd` と `dpkg -S` で割り出して入れる
（OS の版を上げてもパッケージ名を手で直さずに済む）。CI は linux/amd64 でビルドする（worker は約 30 分）。
linux/arm64（Apple Silicon の Mac など）でもビルドできる作りだが、メモリ 8GB の環境では COLMAP・OpenMVS・OpenSplat の
コンパイルが同時に走ってメモリが足りなくなり、確認できていない（`--build-arg BUILD_JOBS=1` で並列数を下げる）。
main に push すると、GitHub Actions が**中身の変わったイメージだけ**を `ghcr.io/ieyoukan/3d-scanner-{web,api,worker}` に
公開する（タグは `latest` と `sha-<コミット>`）。画面だけを直したときに worker が入れ替わらない（処理中のジョブが止まらない）ため。

## Kubernetes への配備（Helm）

```bash
# 構文と出力の確認（イメージのタグは必須。無いと描画を止める）
TAGS="--set web.image.tag=sha-xxxxxxx --set api.image.tag=sha-xxxxxxx --set worker.image.tag=sha-xxxxxxx"
helm lint charts/3d-scanner --set postgresql.auth.existingSecret=x $TAGS
helm template scanner charts/3d-scanner --set postgresql.auth.existingSecret=x $TAGS

# 配備の例（秘密の値は先に Secret として作っておく）
kubectl create namespace 3d-scanner
kubectl -n 3d-scanner create secret generic 3d-scanner-secrets \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 24)"

helm install scanner charts/3d-scanner -n 3d-scanner $TAGS \
  --set postgresql.auth.existingSecret=3d-scanner-secrets \
  --set persistence.storageClass=proxmox-data \
  --set postgresql.persistence.storageClass=proxmox-data \
  --set ingress.enabled=true --set ingress.className=cloudflare \
  --set 'ingress.hosts[0].host=scanner.example.com'
```

> `kubectl` / `helm` を実行する前に、`kubectl config current-context` で対象のクラスタを必ず確認すること。

主な値（全項目は `charts/3d-scanner/values.yaml`）:

| 値 | 内容 |
| --- | --- |
| `web.image.*` / `api.image.*` / `worker.image.*` | 役割ごとのイメージ名とタグ（タグは必須） |
| `web.replicas` / `web.resources` / `api.resources` | 画面と API の Pod の数と資源 |
| `ingress.hosts[].host` / `ingress.className` | 公開するホスト名と IngressClass |
| `persistence.storageClass` / `persistence.size` | 作業領域の StorageClass と容量 |
| `postgresql.enabled` / `externalDatabase.*` | 内蔵の PostgreSQL を使うか、既存の DB に繋ぐか |
| `postgresql.auth.existingSecret` | DB パスワードを入れた Secret の名前 |
| `config.retention` | 保存期間 |
| `config.pipeline.*` | メッシュの作り方、3DGS の反復回数など |
| `gpu.enabled` / `gpu.resourceName` | GPU の割り当て（既定のイメージでは使われない） |
| `resources` | worker の CPU とメモリの要求・上限 |
| `affinity.nodeAffinity` | 載せるノードの条件（3 つの Pod すべて）。api と worker は同じノードに載せる（自動） |
| `networkPolicy.enabled` | worker の内部用 HTTP に api の Pod からだけ届くようにする（ノードの網を `networkPolicy.nodeCIDRs` に） |

Pod の構成:

| Pod | 入れ替え方 | 作業領域（PVC） |
| --- | --- | --- |
| scanner-web | RollingUpdate（止まらない） | 使わない |
| scanner-api | RollingUpdate（止まらない） | 使う |
| scanner-worker | Recreate（処理中のジョブは止まり、次の起動でやり直す） | 使う |

PVC は ReadWriteOnce（同時に 1 ノードからしかマウントできない）なので、api と worker は同じノードに載せる
（podAffinity。チャートが自動で付ける）。Ingress は `/api` を api、それ以外を web に振り分ける。

自宅クラスタ（Argo CD）への登録は `ieyoukan/home-manifests` の `argocd-apps/3d-scanner.yaml` で行う。
仕組みは [docs/learning/06-helm-proxmox-csi.md](docs/learning/06-helm-proxmox-csi.md)。

### 公開するときの注意

- **このアプリにはログイン機能が無い。** 公開する URL の前段に Cloudflare Access などの認証を必ず置くこと
  （置かないと、URL を知っている人は誰でもアップロード・閲覧・削除ができ、CPU とディスクを使われる）。
  アプリは、別サイトのページから発生した POST / DELETE（CSRF）だけは Origin の確認で断る。
- 写真には部屋の中や人が写りうる。保存期間を必要以上に長くしない。
- Cloudflare（Free / Pro）はアップロードを 1 回 100MB で打ち切る。`config.limits.maxUploadMB` は 95 以下にする。

## 検証の記録

2026-11-16 時点。実行していないことは「未検証」と書いている。

### 確認できたこと

| 項目 | 方法 | 結果 |
| --- | --- | --- |
| Go | `go vet ./...`、`go test -race ./...` | 成功（9 パッケージ） |
| 役割の分割と OpenMVS（通し） | CI が公開した amd64 の 3 イメージ（web 63MB・api 15MB・worker 1.7GB）を手元の docker compose（Rosetta）で動かし、1 周の動画（31 枚）を画面と同じ経路（web → api）で送った | worker は COLMAP 4.2.1・OpenMVS ありと判断して `meshMode: openmvs`。SfM 122 秒（31/31 枚、0.42px）→ OpenMVS の密な点群 141 秒（27 万点）→ メッシュ 23 秒（149,945 面、GLB 6.5MB）。ビューアで棒・看板・床が表示された。順番待ちの削除（api → worker）は cancelled、位置合わせ（api → worker）は 3,697 点が一致して成功 |
| 風景モードの照合（組を絞る） | 物の 1 周の動画 2 本（31 枚・40 枚、1 秒に 3 枚・長辺 1600px）で、全部の組と `ScenePairs` と同じ組（前後 8 枚＋16・32 枚離れたもの）を COLMAP 4.2.1 で比べた（手元の Docker、CPU） | どちらも全部の写真の位置が求まった。組 465→227、780→316。照合の時間 74→54 秒、273→221 秒（近い写真どうしの照合に時間の大半がかかるので、30〜40 枚では 2〜3 割しか縮まない。150 枚では全部の組 11,175 に対し約 1,500 組）。点の数は 4%・14% 減った |
| PostgreSQL 実装 | 契約テストを PostgreSQL 17 に対して実行 | 成功 |
| Web | `tsc --noEmit`、`vitest run`、`vite build` | 成功（単体テスト 69 件） |
| Helm | `helm lint`、`helm template`（4 通りの構成）、kubeconform（Kubernetes 1.33、strict） | 成功 |
| コンテナイメージ | linux/arm64 を手元で、linux/amd64 を GitHub Actions でビルド | 成功（約 1.5GB。amd64 は約 20 分） |
| 公開イメージ（amd64） | `ghcr.io/ieyoukan/3d-scanner:sha-e58f622` を認証なしで取得し、パイプラインを実行（arm64 機上のエミュレーション） | 取得できる。SfM → メッシュ → 3DGS（10 反復）→ SOG がすべて成功 |
| SfM（実画像） | サンプル 11 枚を COLMAP 4.2.1 で実行 | 11/11 枚の位置を推定、3D 点 8,348 個、再投影誤差 0.63px、約 1 分 |
| メッシュ（簡易） | `delaunay_mesher`（疎）→ `mesh_texturer` → GLB | 頂点 4,586・面 9,166、3.7MB、約 2 秒 |
| 3DGS（CPU） | OpenSplat 300 / 700 反復 | 7 分 23 秒 / 10 分 11 秒。PLY 2.0MB |
| SOG | `splat-transform -g cpu` | 2.0MB → 約 110KB、1 秒 |
| サーバー経由の一連の流れ | Docker Compose で起動し、ブラウザ（スマホ幅）で操作 | （当時はトークンのログインあり）写真選択 → 確認・削除 → 送信 → 処理状況 → 3 つのタブで 3D 表示 → 保存 |
| 部分成功 | 3D 点が極端に少ない入力 | メッシュ失敗・3DGS 成功 → `partial`。失敗理由を表示し、タブは無効で残る |
| SfM の失敗 | 模様の無い単色画像 | `failed`。後段は「実行せず」、成果物なし |
| キャンセル | SfM 実行中に DELETE | `cancelled`。外部プロセスが止まり、作業領域が削除される |
| 再起動からの回復 | 実行中にコンテナを再起動 | 起動時に待ち行列へ戻り、2 回目の着手で完了 |
| 非 root・読み取り専用 | Compose の `read_only: true`、UID 10001 | 動作 |
| Helm chart の実インストール | 使い捨ての k3s v1.33.6（Docker 上、StorageClass は `local-path`）に `helm install` | Pod 2 つが Running、プローブ通過、PVC 2 つが Bound。UID 10001・読み取り専用ルート FS のまま復元ジョブが成功 |
| Kubernetes 上での回復 | 同じ k3s で、ジョブ実行中に Pod を削除 | 新しい Pod が同じ PVC で起動し、中断ジョブを 2 回目の着手で完了。以前のスキャンも保持 |
| カメラ経路 | `getUserMedia` を合成映像に差し替えて撮影 | 映像表示・撮影・取り込みが動作。拒否時は写真選択に切り替わる |
| 位置合わせ（VPS）（2026-11-18） | サンプル 7 枚で作った地図に、地図に含まれない 1 枚を `POST /localize`（COLMAP 4.2.1、arm64） | 元の寸法（カメラを使い回す）: 一致 1,492 点・14.5 秒。長辺 2048 に縮小（焦点距離を推定）: 一致 1,421 点・15.1 秒で、位置の差 0.03。無関係な画像: 「一致しない」・9.9 秒 |
| 続きを撮る（2026-11-18） | 旧版（ギャラリー機能の版）で作ったスキャンに、新版で 4 枚を追加 | マイグレーション 0003 を既存 DB に適用。4 枚中 4 枚を追加し全体 11 枚・3D 点 8,335 個、メッシュ・3DGS・SOG まで成功。前の版のカメラ位置の差は最大 1.8e-15（固定されている）。事前の位置合わせの位置と、作り直し後の同じ写真のカメラ位置の差は 0.013（被写体の広がり 4.7） |
| 続きを撮る画面 | ブラウザ（幅 390px）で、ギャラリー → ビューア → 続きを撮る → 写真 1 枚 → 自動の位置合わせ → 送信 | 地図に前回の撮影位置と今回の位置・向きを表示。送信後は新しい版の処理画面に移り、完成後のギャラリーには最新の版だけが出る。コンソールのエラーなし |
| 3DGS の後回し（2026-11-18） | サンプル 11 枚を送り、学習中に 6 枚の別スキャンを送る（COLMAP 4.2.1・OpenSplat CPU、arm64） | 11 枚はメッシュまで 69 秒で完成（保存済み）、3DGS は待ち行列へ。別スキャンの受付直後に学習が止まって順番待ちに戻り、別スキャンがメッシュまで 22 秒で完成した後に学習をやり直した（11 枚 186 秒、6 枚 108 秒、200 反復）。作り直し（`POST /splat`）は前の 3DGS を配信したまま学習し、終わったら置き換わった |
| 焦点距離の手掛かり | EXIF を消した同じ写真に、メタデータで 35mm 換算 35mm を付けて送る | COLMAP の初期値が 2865.58px（事前値）になった。EXIF つきの元の写真から COLMAP が出した値は 2865.36px。付けないと長辺の 1.2 倍（3398px）から始まる |
| 自動撮影 | ブラウザのカメラを、写真を 1.5 秒かけて横に流しては 1.5 秒止まる合成映像に差し替える | 約 20 秒で 12 枚を自動で撮影し、同じ景色の重複は無し。静止画（ImageCapture）は合成映像では撮れず、映像の 1 コマに切り替わった。手ぶれ（幅の ±0.6%）を加えて「1.2 秒動いて 2 秒止まる」を 8 回繰り返すと、修正前の判定は 4 枚、止まる基準を 2 段にした後は 12 枚 |
| 動画を選ぶ | 歩きながら 1 周撮った動画 3 本（Android、縦 1080×1920、10〜13 秒）を Chrome で読み込ませ、そのまま送って COLMAP で復元 | 11〜15 秒で 31 / 37 / 40 枚を選んだ。全部の写真の位置を推定でき、最初と最後の写真もつながった（再投影誤差 0.40 / 0.44 / 0.48px、1 周の中の最大の抜け 29 / 19 / 18°）。ffmpeg で同じ選び方をした場合と同じ程度。読み込み中にページを 2.5 秒固まらせると 7 区間でコマが届かなかったが、再生後のシークで埋めて 37 枚そろった |
| 動画で撮る | ブラウザのカメラを、白い机の 1 周動画（12 秒）を流す映像に差し替えて録画ボタンで撮影し、そのまま送って復元 | 12 秒で 37 枚。全部の写真の位置を推定でき、1 周がつながった（3D 点 32,647、再投影誤差 0.42px、最大の抜け 20°）。動画を選んだ場合と同じ程度。13 秒の動画を繰り返し流して 35 秒撮ると、26 秒で 77 枚から 40 枚台に間引かれ、停止時は 54 枚（上限 80 枚に収まった） |

### 未検証のこと

| 項目 | 理由 | 確認の手順 |
| --- | --- | --- |
| COLMAP の密な復元（MVS）によるメッシュ | CUDA 対応の GPU が無い。単体テストでコマンドの順番と引数だけ確認 | CUDA 入りのイメージを作り、`MESH_MODE=dense` で実行する（[docs/learning/03-mvs.md](docs/learning/03-mvs.md)） |
| OpenMVS の amd64 での動作とテクスチャの継ぎ目 | 手元（arm64）では 1 周の動画 2 本で動いた。継ぎ目をならす処理（seam leveling）は arm64 のビルドで壊れたので既定で切ってある。本番（amd64）では未確認 | 本番で 1 件作り、メッシュの見た目と処理時間を確かめる。`OPENMVS_SEAM_LEVELING=true` で継ぎ目が直るか試す |
| 役割の分割（web・api・worker）の本番での動作 | 手元の docker compose では確かめた（上の「確認できたこと」）。本番の Ingress（Cloudflare）の /api の振り分けと、api と worker が同じノードに載るかは未確認 | 配備後に 3 つの Pod が Running か、api と worker が同じノードか、画面と API が両方開くかを見る |
| GPU での 3DGS 学習 | 同上 | CUDA 版の OpenSplat を入れ、`SPLAT_FORCE_CPU=false` |
| スマホ実機（Android Chrome）での撮影 | 開発環境から実機に繋いでいない | HTTPS の URL を用意し、実機で撮影から閲覧まで通す |
| amd64 の実機での所要時間 | amd64 イメージの動作はエミュレーションで確認したが、処理時間は実機と異なる | 配備後に、実際の所要時間を測る |
| 本番クラスタへの配備 | 実際のクラスタには適用していない。使い捨ての k3s では確認したが、Proxmox CSI のボリューム、Cloudflare の Ingress、SealedSecret の復号は本番でしか確かめられない | home-manifests の Pull Request をマージし、Pod・PVC・Ingress を確認する |
| Cloudflare Tunnel 経由のアップロード | 未配備 | 80 枚程度を実機から送り、413 / 524 が出ないことを確認する |
| 20 枚を超える写真での処理時間とメモリ | サンプルは 11 枚 | 枚数を増やして、所要時間とメモリ使用量を測る |
| スマホ実機で日を改めて撮った写真の位置合わせ | サンプル（同じ日・同じカメラ）でしか試していない。照明・季節・物の配置が変わると一致しにくくなる | 実機で保存したスキャンの場所に戻り、続きを撮る画面で位置合わせする |
| 自動撮影のしきい値（実機） | 合成映像とサンプル写真でしか試していない。手ぶれ・歩く速さ・照明で「止まっている」「景色が変わった」の判定が変わる | 実機で 1 周撮り、撮れた枚数と SfM で位置を推定できた枚数を見て `defaultAutoConfig` を調整する |
| AR で撮る（実機） | 手元のブラウザでは AR を動かせないため、計算（タイルの判定・左右の案内）のテスト、AR が無い／始められないときの表示、AR の画面の見た目（写真を背景にした確認）までしか試していない。カメラ画像の上下の向き、ドームの位置、撮影中の速さは実機で確かめる必要がある | Android の Chrome で AR を選び、1 周撮って、確認画面の写真の向き・枚数と SfM の結果を見る |
| 風景モード（実機） | 物の 1 周の動画 2 本で、照合の組を絞っても全部の写真の位置が求まることは確かめた（下の「確認できたこと」）。歩きながら空間を撮った写真では試していない。AR の歩いた道・警告は計算のテストと見た目だけ | 部屋か庭を「風景」で歩きながら撮り、SfM の説明（位置が求まった枚数、つながらなかった所）を見る。警告のしきい値（35°・12cm・秒速 0.8m）を調整する |
| 続きを撮るの AR（実機） | 重ね方の計算（大きさが分かれば 1 回、分からなければ離れた 2 か所、真下を向いた位置合わせは方位に使わない）のテストと、画面の見た目までしか試していない。位置合わせ 1 回に本番で 50〜60 秒かかるので、その間同じ所を映し続ける必要がある | Android の Chrome で保存済みのスキャンの続きを AR で撮り、前回のメッシュが実物に重なるか、前回の方向が緑になるかを見る |
| AR で見るの切り抜き（実機） | 「すごい速さで動いてどこかへ行く」の原因（背景の膜）は、手元で 1 周の動画 2 本からメッシュを作って確かめた。切り抜いた後の見え方は実機で見ていない | 「AR で見る」で机に置き、対象だけが止まって見えるか、大きさが机に乗る程度かを見る |
| AR で見る・実際の大きさ（実機） | AR は手元で動かせないため、置き方の計算（真上・底・正面・大きさ）と、カメラ位置の突き合わせ（既知の回転・縮尺を復元できるか、合わせられないときに断るか）のテスト、AR で見る画面の見た目までしか試していない | Android の Chrome で AR で撮って送り、SfM の説明に「実際の大きさ」が出るか、「AR で見る」で机に置いた大きさが実物と合うかを見る |
| 動画で撮る・動画を選ぶ（実機） | Chrome（Mac）でしか試していない。「動画で撮る」の検証は動画を流した映像で、本物のカメラの画質（手ぶれ・自動露出・ピント）ではない。スマホで撮りながら毎コマ測る負荷、読み込み速度・メモリ、iPhone の HEVC 動画 | Android と iPhone で 1 周撮り、枚数・所要時間・SfM の結果を見る |
| 白い天板・細い支柱 | 模様の無い面や細い物は点が取れず、GPU 無しの簡易メッシュでは平面になる・消える（白い机の動画では、天板の高さに点が 1 つも無かった） | 天板に模様のある物を置いて撮る。GPU を入れて密な復元を使う |
| 静止画での撮影（ImageCapture） | 実機で試していない。端末によっては 1 枚に 1〜2 秒かかる、プレビューが一瞬止まる | Android の Chrome で「静止画」を入れて撮り、時間と画質を確かめる |
| 大きな地図での位置合わせの時間 | 地図の写真は 7〜11 枚で確認。突き合わせは最大 60 枚に絞っている | 60 枚以上のスキャンで `durationMs` を測る。前段（Cloudflare）の約 100 秒に収まるか確かめる |

## 使用している OSS とライセンス

| ソフトウェア | 用途 | ライセンス |
| --- | --- | --- |
| [COLMAP](https://github.com/colmap/colmap) 4.2.1 | SfM、メッシュ化、テクスチャ | BSD-3-Clause（LSD は無効にしてビルド） |
| [OpenSplat](https://github.com/WebODM/OpenSplat) 1.2.2 | 3D Gaussian Splatting の学習 | **AGPL-3.0** |
| [LibTorch](https://pytorch.org/) 2.13 | OpenSplat が使う数値計算 | BSD-3-Clause |
| [meshoptimizer / gltfpack](https://github.com/zeux/meshoptimizer) 1.3 | OBJ → GLB | MIT |
| [SplatTransform](https://github.com/playcanvas/splat-transform) 3.11 | PLY → SOG | MIT |
| [PlayCanvas Engine](https://github.com/playcanvas/engine) | 3DGS の表示 | MIT |
| [three.js](https://github.com/mrdoob/three.js) | メッシュと点群の表示 | MIT |
| [React](https://react.dev/)、[React Router](https://reactrouter.com/) | 画面 | MIT |
| [Vite](https://vite.dev/)、[Vitest](https://vitest.dev/) | ビルド、テスト | MIT |
| [Tailwind CSS](https://tailwindcss.com/) | スタイル | MIT |
| [pgx](https://github.com/jackc/pgx) | PostgreSQL ドライバ | MIT |
| [PostgreSQL](https://www.postgresql.org/) | DB | PostgreSQL License |
| [tini](https://github.com/krallin/tini) | コンテナの PID 1 | MIT |

**OpenSplat（AGPL-3.0）について**: このアプリは OpenSplat を別プロセスとして実行するだけで、コードは取り込んでいない。
ただし AGPL には「ネットワーク越しに利用させる場合もソースを提供する」という条項があるので、
OpenSplat を改変して第三者に公開する場合は、その改変版のソースを提供する必要がある。
公開範囲を広げる前に、ライセンスの原文を確認すること。

このリポジトリ自体のライセンスは未設定（決めたら `LICENSE` を追加する）。

サンプル写真（`scripts/fetch-sample.sh` が取得するもの）は openMVG の
[ImageDataset_SceauxCastle](https://github.com/openMVG/ImageDataset_SceauxCastle)
（Copyright 2012 Pierre MOULON）。ライセンスの明記が無いため、このリポジトリには含めていない。
