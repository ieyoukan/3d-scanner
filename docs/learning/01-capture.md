# 01. 撮影 — 写真を集めてサーバーへ送る

## このノートで分かること

- ブラウザだけでカメラを使う方法と、使えないときに行き止まりにしない作り方
- 写真を「送る前に」縮小・点検する理由（Cloudflare の制限、ぼけた写真の害）
- アップロードをサーバーがどう検証しているか（信用できない入力の扱い）
- どこまで実機で確かめ、どこが未検証か

## 目的

復元（SfM → メッシュ / 3DGS）に使える写真を集めて、サーバーに渡す。

3D 復元の成否は、ほぼ写真で決まる。後段のアルゴリズムは「複数の写真に同じ点が写っている」ことだけを
手掛かりにするので、重なりの無い写真・ぶれた写真・模様の無い被写体からは何も復元できない。
だからこの段階の仕事は、写真を送ることに加えて「使えない写真を送る前に気づかせる」ことにある。

## 入力

2通りある。どちらも同じ `Frame`（`web/src/capture/frames.ts`）になり、以降の扱いは同じ。

| 入力元 | 取得方法 | 特徴 |
| --- | --- | --- |
| ページ内のカメラ | `navigator.mediaDevices.getUserMedia` | 連続して撮れる。HTTPS（または localhost）が必須。EXIF は付かない |
| 端末の写真 | `<input type="file" accept="image/*" multiple>` | HTTP でも使える。元ファイルのまま送れれば EXIF（焦点距離）が残る |

撮り方の目安（Polycam の Object Mode の案内を 2026-11-16 にブラウザで確認）:
最低 20 枚、隣の写真との重なり 70〜75%、対象のまわりを 15〜30 度ずつ、高さを変えて複数周。
このアプリの下限はサーバー設定の `MIN_IMAGES`（既定 8）、推奨は `recommendedImages = 20`
（`web/src/capture/validation.ts`）。

## 処理

```text
[カメラ]  getUserMedia → <video> → <canvas> → JPEG(品質0.92) ─┐
                                                             ├→ Frame { blob, thumbUrl, sharpness, ... }
[ファイル] <input type=file> → 必要なら縮小して JPEG 化 ──────┘        │
                                                                     v
                              一覧で確認・削除（FrameGallery）→ 検証（checkFrames）
                                                                     │
                                                                     v
                         XHR で multipart 送信（uploadScan）→ サーバーが 1 枚ずつ検証（upload.Receive）
                                                                     │
                                                                     v
                                              202 Accepted { id, status: "queued" }
```

### 1. カメラの許可と状態遷移

`web/src/camera/useCamera.ts` が次の状態を持つ。

```text
idle ──start()──▶ starting ──成功──▶ ready ──stop()──▶ idle
                     └──失敗──▶ unavailable（理由つき）
```

要点は3つ。

- **呼ぶ前に調べる。** HTTPS でないページでは `navigator.mediaDevices` 自体が存在しない（MDN）。
  `checkCameraSupport`（`web/src/camera/cameraErrors.ts`）が `window.isSecureContext` を先に見て、
  「HTTPS ではないため使えない」と説明する。
- **希望として条件を出す。** `facingMode: { ideal: 'environment' }`、`width: { ideal: 1920 }`、
  `height: { ideal: 1440 }`。`exact` にすると背面カメラの無い PC で `OverconstrainedError` になる。
- **失敗を分類して、次の行動を示す。** `describeCameraError` が DOMException の `name` で分ける。

| name | 分類 | 画面の案内 |
| --- | --- | --- |
| `NotAllowedError` / `SecurityError` | denied | 鍵アイコンから許可するか、写真を選ぶ |
| `NotFoundError` / `OverconstrainedError` | not-found | 写真を選ぶ |
| `NotReadableError` / `AbortError` | in-use | 他のアプリがカメラを使っていないか確認 |

どの場合も「写真を選ぶ」ボタンは押せる（`web/src/capture/CaptureView.tsx`）。カメラは便利な入力手段の
1つであって必須ではない、という作りにしてあるので、権限の拒否や HTTP 接続でも続けられる。

ページを開いた瞬間には許可を求めない。Permissions API で「以前に許可済み」と分かったときだけ自動で開始し、
そうでなければ説明を読んでから「カメラを開始」を押してもらう（理由: 文脈の無い許可ダイアログは拒否されやすい）。

タブを裏に回すとカメラを止め、戻ると再開する（`visibilitychange`）。止めるときは
`track.stop()` を必ず呼ぶ。呼ばないと端末のカメラ使用中ランプが点いたままになる。

### 2. 1 枚を取り込む（Frame を作る）

`frameFromCapture` / `frameFromFile`（`web/src/capture/frames.ts`）。1 枚につき3つを用意する。

| 項目 | 中身 | 用途 |
| --- | --- | --- |
| `blob` | 送信する本体（JPEG か PNG） | アップロード |
| `thumbUrl` | 長辺 320px のサムネイル | 一覧表示 |
| `sharpness` | ラプラシアンの分散 | ぼけ判定 |

サムネイルを別に作るのはメモリのため。ブラウザは `<img>` を表示するとき画像を画素に展開する。
1920×1440 の写真は展開すると約 11MB で、80 枚並べるとスマホのタブが落ちうる。

ファイルから取り込むときの判断:

```text
JPEG/PNG で、長辺 ≤ 2048px、かつ 3MB 以下  →  元のファイルをそのまま送る（EXIF が残る）
それ以外（HEIC、巨大な写真など）            →  canvas で長辺 2048px の JPEG(品質0.9) に作り直す（EXIF は失われる）
```

EXIF を残したいのは、焦点距離が入っていると COLMAP がカメラの初期値に使えるため（→ 02）。
一方で縮小が必要なのは、Cloudflare（Free / Pro）がリクエスト本文を 100MB で打ち切るため。
実測では、サンプル 11 枚（元は 2832×2128、合計 約12MB）が縮小後に合計 6.4MB になった。

展開には `createImageBitmap(blob, { imageOrientation: 'from-image' })` を使う。スマホを縦に構えた写真は
「横長の画素＋回転の指定」で保存されていることが多く、この指定が無いとサムネイルが横倒しになる。

### 3. ぼけ判定（ラプラシアンの分散）

`web/src/capture/sharpness.ts`。グレースケールにした縮小画像の各画素について

```text
L(x, y) = 上 + 下 + 左 + 右 − 4 × 中央        （2階微分の近似）
くっきり度 = L の分散
```

輪郭がはっきりしていれば L は場所によって大きく振れ（分散が大きい）、ぼけていればどこでも 0 に近い。

この値は被写体の模様にも左右される（無地の壁はくっきり撮っても低い）。そこで絶対値では判定せず、
`findBlurry` が「同じ撮影の中央値の 35% 未満」のものだけに「ぼけ?」の印を付ける（5 枚以上あるとき）。
基準に平均でなく中央値を使うのは、極端な 1 枚に基準が引っ張られないようにするため。

### 4. 送信前の確認

`checkFrames`（`web/src/capture/validation.ts`）が「開始できるか」と理由を返す。

- error（開始できない）: 枚数が下限未満・上限超過、合計サイズが `maxUploadBytes` 超過
- warn（開始できるが助言）: 20 枚未満、ぼけの疑いがある写真がある

上限値は画面に書き写さず、`GET /api/config` でサーバーから受け取る。片方だけ変えて食い違うのを防ぐため。
ただし画面の確認は開発者ツールで迂回できるので、同じ検証をサーバーでも必ず行う（次項）。

### 5. アップロード

`uploadScan`（`web/src/upload/uploadScan.ts`）が `multipart/form-data` を組み立てる。

```text
images    写真（枚数ぶん繰り返す）
metadata  JSON: { client: {...}, frames: [{ capturedAt, source, width, height }, ...] }
```

`fetch` ではなく `XMLHttpRequest` を使うのは、送信の進み具合（`upload.onprogress`）を取れるから。
画面の % は「送れたバイト数 ÷ 全体」で、作り物ではない。

### 6. サーバー側の検証

入口は `handleCreateScan`（`server/internal/httpapi/handlers.go`）。本文を読む**前に**断れるものを先に見る。

```text
同時アップロードの席（2つ）が空いているか     → 無ければ 429 busy
処理待ち＋実行中が上限未満か（MAX_QUEUED_SCANS） → 超えていれば 429 queue_full
PVC の空きが足りるか（MIN_FREE_DISK_MB）        → 足りなければ 507 disk_full
http.MaxBytesReader で本文全体に上限（MAX_UPLOAD_MB、既定 95MB）
upload.Receive で 1 枚ずつ検証しながら保存
DB に登録 → ワーカーを起こす → 202
```

`Receive` / `saveImage`（`server/internal/upload/upload.go`）は 1 枚ごとに次を確かめる。

| 検査 | 方法 | 防ぐもの |
| --- | --- | --- |
| 種類 | 先頭 512 バイトを `http.DetectContentType` で判定（拡張子や申告は見ない） | 画像のふりをした別物 |
| サイズ | `io.LimitReader(上限+1)` で読み、超えたら拒否 | 巨大ファイル |
| 画素数 | `image.DecodeConfig`（ヘッダだけ読む）で幅×高さ | 小さいのに展開すると巨大な「画像爆弾」 |
| 重複 | 読みながら SHA-256 を計算し、同じ内容は保存しない | 同じ写真の二重送信 |
| 名前 | サーバーが `frame_0001.jpg` と付ける | ファイル名に混ぜた `../` |

`r.ParseMultipartForm` を使わないのは、あちらは全項目を読み終えてから返すため。1 項目ずつ読めば、
問題のある項目に当たった時点で残りを読まずに断れる。失敗時は作業ディレクトリごと消す。

## 出力

- PVC: `/data/scans/<id>/images/frame_0001.jpg` …
- DB: `scans` 1 行（`status = 'queued'`）、`scan_frames`（寸法・SHA-256・`captured_at`・`camera` JSON）、
  `scan_steps`（5 行、すべて `pending`）。`captured_at` と `camera` は MVP では使わないが、
  再訪問スキャンや VPS を足すときの土台（`server/internal/store/migrations/0001_init.sql`）。
- 応答: `202 Accepted` と `{"id": "...", "status": "queued", "frameCount": 11, ...}`。
  200 でなく 202 なのは、復元が終わるまで応答を待たせると Cloudflare が約 125 秒で 524 を返すため。
  手元の実測では、11 枚の受付は 0.06 秒で返った。

## よくある失敗

| 症状 | 原因 | 対処 |
| --- | --- | --- |
| カメラのボタンを押しても始まらない | HTTP（LAN の IP アドレスなど）で開いている | HTTPS で開くか「写真を選ぶ」 |
| 「許可されていません」 | 以前に拒否した設定が残っている | アドレスバーの鍵アイコンから許可 |
| `too_few_images` なのに枚数は足りているはず | 同じ内容の写真が除かれた。検証中、同一内容の 8 枚を送ると 1 枚と数えられ 400 になった | 違う位置から撮った写真を送る |
| `unsupported_type` | HEIC など。ファイル選択経由なら画面側で JPEG に変換されるが、curl で直接送ると拒否される | JPEG / PNG にする |
| `upload_too_large`（413） | 合計が上限超過 | 枚数を減らす。画面経由なら自動で縮小される |
| 一覧の向きが横倒し | EXIF の回転を無視して展開した | `imageOrientation: 'from-image'` を付ける |
| ページを更新したら写真が消えた | 写真はメモリ上にしか無い（MVP の仕様）。離脱前に確認ダイアログは出る | 送信してから移動する |

## 確認方法

```bash
# 画面側のロジック（ぼけ判定・検証・エラー分類）の単体テスト
cd web && npm test

# サーバー側の検証（偽装・巨大画像・重複など）の単体テスト
cd server && go test ./internal/upload/ ./internal/httpapi/

# API を直接叩く（docker compose で起動済み、トークンは .env の ACCESS_TOKEN）
./scripts/fetch-sample.sh
args=(); for f in samples/sceaux/images/*.JPG; do args+=(-F "images=@$f"); done
curl -s -X POST http://localhost:8080/api/scans -H "Authorization: Bearer $ACCESS_TOKEN" "${args[@]}"
```

この開発時点で確かめたこと・確かめていないこと:

- 確認済み: ファイル選択 → 確認 → 削除 → 送信の一連（デスクトップの Chromium、幅 390px）。
- 確認済み（スタブ）: `getUserMedia` を「動く模様を描いた canvas の映像」を返す関数に差し替え、
  映像表示・シャッター・取り込み（9 枚）を確認。`NotAllowedError` を投げる関数に差し替え、拒否時の案内を確認。
- **未検証**: Android Chrome の実機での撮影（本物のカメラ、許可ダイアログ、端末の回転、メモリ）。
  実機で試すには HTTPS が要る（Cloudflare Tunnel 経由、または `localhost` への USB ポート転送）。

## 次の処理

受け付けられたスキャンは待ち行列に入り、ワーカーが 1 件ずつ取り出して SfM から始める。

## 次に読む

- [02-sfm.md](02-sfm.md) — 写真からカメラの位置と疎な点群を求める
- [05-go-jobs.md](05-go-jobs.md) — 待ち行列とワーカーの仕組み
