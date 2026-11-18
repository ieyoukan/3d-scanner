// Package scan は「スキャン（＝1回の3D復元ジョブ）」を表す型と、その状態の決まりを定義する。
//
// このパッケージは他のどのパッケージにも依存しない。HTTP も DB も外部CLI も知らない。
// 「状態がどう決まるか」というルールだけをここに集めておくと、テストが書きやすく、
// 画面・DB・ワーカーが同じ言葉（queued / running / partial ...）で会話できる。
package scan

import (
	"encoding/json"
	"time"
)

// Status はスキャン全体の状態。API の status フィールドと DB の scans.status に入る。
type Status string

const (
	// StatusQueued: 画像を受け取り、ワーカーが空くのを待っている。
	StatusQueued Status = "queued"
	// StatusRunning: ワーカーが外部CLIを実行中。
	StatusRunning Status = "running"
	// StatusPartial: SfM は成功し、見られる3Dデータが少なくとも1つあるが、失敗したステップもある。
	// 例: Mesh は出来たが 3DGS は失敗した。
	StatusPartial Status = "partial"
	// StatusSucceeded: 実行したステップがすべて成功した。
	StatusSucceeded Status = "succeeded"
	// StatusFailed: 見られる3Dデータ（Mesh / 3DGS）が1つも出来なかった。
	StatusFailed Status = "failed"
	// StatusCancelled: 利用者が中止した。
	StatusCancelled Status = "cancelled"
	// StatusExpired: 保存期限が過ぎ、画像と成果物を削除した。行だけがしばらく残る。
	StatusExpired Status = "expired"
)

// Terminal は「これ以上ワーカーが状態を変えない」終了状態かどうかを返す。
func (s Status) Terminal() bool {
	switch s {
	case StatusPartial, StatusSucceeded, StatusFailed, StatusCancelled, StatusExpired:
		return true
	}
	return false
}

// Active は処理待ちまたは処理中（＝キャンセルできる状態）かどうかを返す。
func (s Status) Active() bool {
	return s == StatusQueued || s == StatusRunning
}

// StepName は復元パイプラインの1段階の名前。
type StepName string

const (
	// StepSfM: Structure from Motion。写真からカメラの位置・向きと疎な点群を求める。全員の入力。
	StepSfM StepName = "sfm"
	// StepMVS: Multi-View Stereo。画素ごとの奥行きを求めて密な点群を作る（COLMAP では CUDA が必要）。
	StepMVS StepName = "mvs"
	// StepMesh: 点群から三角形メッシュを作り、テクスチャを貼り、GLB に変換する。
	StepMesh StepName = "mesh"
	// StepSplat: OpenSplat で 3D Gaussian Splatting を学習し、PLY を出力する。
	StepSplat StepName = "splat"
	// StepSOG: PLY を Web 配信向けの SOG 形式に圧縮する。
	StepSOG StepName = "sog"
)

// StepOrder は画面に並べる順番であり、DB に行を作る順番でもある。
// 実行順は reconstruction パッケージが決める（sfm →（mvs → mesh）→（splat → sog））。
var StepOrder = []StepName{StepSfM, StepMVS, StepMesh, StepSplat, StepSOG}

// StepStatus は1ステップの状態。
type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	// StepSkipped: 実行しなかった。理由は Step.Message に日本語で入れる
	// （例: 設定で無効、GPU が無い、前段が失敗した）。失敗とは区別する。
	StepSkipped StepStatus = "skipped"
)

// AssetKind は配信できる成果物の種類。URL の /assets/{kind} にそのまま使う。
type AssetKind string

const (
	// AssetSparsePLY: SfM の疎な点群（色付きの点の集まり）。
	AssetSparsePLY AssetKind = "sparse_ply"
	// AssetSceneJSON: SfM 結果の要約（上方向・中心・カメラ位置）。ビューアが初期視点を決めるのに使う。
	AssetSceneJSON AssetKind = "scene_json"
	// AssetMeshGLB: テクスチャ付きメッシュ（glTF バイナリ）。
	AssetMeshGLB AssetKind = "mesh_glb"
	// AssetGaussianPLY: 3DGS の学習結果（ガウシアンの集まり。メッシュではない）。
	AssetGaussianPLY AssetKind = "gaussian_ply"
	// AssetGaussianSOG: 3DGS を Web 向けに圧縮したもの。
	AssetGaussianSOG AssetKind = "gaussian_sog"
)

// AssetInfo は成果物の種類ごとの配信情報。
type AssetInfo struct {
	// ContentType は HTTP の Content-Type。
	ContentType string
	// DownloadName はダウンロード時のファイル名。
	DownloadName string
}

// Assets は「知っている成果物の種類」の一覧表。
// API は URL の {kind} をこの表で検証するので、表に無い文字列でファイルを読まれることはない。
var Assets = map[AssetKind]AssetInfo{
	AssetSparsePLY:   {ContentType: "application/octet-stream", DownloadName: "sparse-points.ply"},
	AssetSceneJSON:   {ContentType: "application/json", DownloadName: "scene.json"},
	AssetMeshGLB:     {ContentType: "model/gltf-binary", DownloadName: "mesh.glb"},
	AssetGaussianPLY: {ContentType: "application/octet-stream", DownloadName: "splat.ply"},
	AssetGaussianSOG: {ContentType: "application/octet-stream", DownloadName: "scene.sog"},
}

// Scan はスキャン1件。DB の scans テーブル1行に対応する。
type Scan struct {
	ID         string
	Status     Status
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ExpiresAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	FrameCount int
	// Attempts はワーカーがこのジョブに着手した回数。再起動で中断されたジョブを
	// 何度でもやり直して無限ループしないための歯止めに使う。
	Attempts     int
	ErrorCode    string
	ErrorMessage string
	// CaptureMeta は撮影側が送ってきた付加情報（端末・カメラ等）を JSON のまま保持する。
	// MVP では使わないが、VPS/ARCore を足すときに列を増やさず項目を追加できる。
	CaptureMeta json.RawMessage
	// Saved はギャラリーに保存されているか。true なら保存期限（ExpiresAt）を過ぎても消さない。
	Saved bool
	// Title は利用者が付けた名前（空なら画面は撮影日時を出す）。
	Title string
	// ParentID は「続きを撮る」で作ったスキャンの元になった版の ID。
	// 新規のスキャンと、元の版が削除されたものは空。
	ParentID string
	// BaseFrameCount は元の版から引き継いだ写真の枚数（FrameCount に含まれる）。
	// FrameCount - BaseFrameCount が、今回追加した写真の枚数。
	BaseFrameCount int
}

// MaxTitleLength はスキャンの名前の最大文字数（DB の CHECK 制約と同じ値）。
const MaxTitleLength = 80

// Summary はギャラリーの一覧に出す1件。本体と、登録済みの成果物の種類だけを持つ
// （ステップの詳細は一覧では使わないので読まない）。
type Summary struct {
	Scan       Scan
	AssetKinds []AssetKind
}

// Frame はアップロードされた写真1枚。DB の scan_frames テーブル1行に対応する。
type Frame struct {
	Index    int
	FileName string // サーバーが付けた名前（frame_0001.jpg）。利用者のファイル名は使わない。
	Width    int
	Height   int
	Bytes    int64
	SHA256   string
	// CapturedAt は撮影時刻（クライアント申告）。再訪問スキャンや時系列の並べ替えに使える。
	CapturedAt *time.Time
	// Camera はカメラ情報の JSON（焦点距離、将来は ARCore の姿勢など）。
	Camera json.RawMessage
}

// Step は1ステップの進行状況。DB の scan_steps テーブル1行に対応する。
type Step struct {
	Name       StepName
	Status     StepStatus
	StartedAt  *time.Time
	FinishedAt *time.Time
	// Message は成功・スキップ時の補足（「42枚中40枚の位置を推定」など）。
	Message string
	// ErrorMessage は失敗の理由（画面にそのまま出す日本語）。
	ErrorMessage string
}

// Asset は成果物1件。DB の scan_assets テーブル1行に対応する。
type Asset struct {
	Kind AssetKind
	// RelativePath はスキャンの作業ディレクトリからの相対パス。絶対パスは DB に入れない
	// （PVC のマウント先を変えても壊れず、ルート外を指す値を拒否しやすい）。
	RelativePath string
	SizeBytes    int64
}

// Detail は API が返すスキャンの全体像（本体＋ステップ＋成果物）。
type Detail struct {
	Scan   Scan
	Steps  []Step
	Assets []Asset
}

// Viewable は「3Dビューアで見られる成果物」かどうか。
// 疎な点群（sparse_ply）は SfM の確認用であり、これだけでは完成とみなさない。
func (k AssetKind) Viewable() bool {
	return k == AssetMeshGLB || k == AssetGaussianPLY || k == AssetGaussianSOG
}

// Outcome は Summarize の結果。
type Outcome struct {
	Status       Status
	ErrorCode    string
	ErrorMessage string
	// Saved はギャラリーに保存するか。Summarize は決めない（設定によるため、呼び出し側が入れる）。
	Saved bool
}

// Keepable は、この終了状態のスキャンをギャラリーに保存する価値があるか（見られる3Dデータがあるか）。
func (s Status) Keepable() bool {
	return s == StatusSucceeded || s == StatusPartial
}

// Summarize は、全ステップが終わった時点のステップ状態と成果物から、スキャン全体の状態を決める。
//
// 入力: 終了済みのステップ一覧と、検証を通って登録された成果物の種類。
// 出力: succeeded / partial / failed のいずれかと、失敗時の説明。
// 副作用: なし（純粋関数）。だから表形式のテストで全パターンを確かめられる。
//
// ルール:
//  1. 見られる成果物（Mesh か 3DGS）が1つも無ければ failed。
//     「API は成功したが中身は空」を成功と呼ばないための最重要ルール。
//  2. 失敗したステップが1つも無ければ succeeded（skipped は失敗に数えない）。
//  3. それ以外（見られるものはあるが、どこかが失敗）は partial。
func Summarize(steps []Step, assets []AssetKind) Outcome {
	var failed []Step
	for _, st := range steps {
		if st.Status == StepFailed {
			failed = append(failed, st)
		}
	}
	viewable := false
	for _, k := range assets {
		if k.Viewable() {
			viewable = true
			break
		}
	}

	if !viewable {
		out := Outcome{Status: StatusFailed, ErrorCode: "no_output", ErrorMessage: "3Dデータを1つも作れませんでした。"}
		if len(failed) > 0 {
			// 最初に失敗したステップが原因であることが多いので、その理由を前面に出す。
			out.ErrorCode = string(failed[0].Name) + "_failed"
			out.ErrorMessage = failed[0].ErrorMessage
		}
		return out
	}
	if len(failed) == 0 {
		return Outcome{Status: StatusSucceeded}
	}
	return Outcome{
		Status:       StatusPartial,
		ErrorCode:    string(failed[0].Name) + "_failed",
		ErrorMessage: "一部の処理が失敗しました。成功した出力は閲覧できます。",
	}
}
