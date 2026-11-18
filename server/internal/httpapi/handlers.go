package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/thumbnail"
	"github.com/ieyoukan/3d-scanner/server/internal/upload"
	"github.com/ieyoukan/3d-scanner/server/internal/vps"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// ---------------------------------------------------------------------------
// JSON の入出力
// ---------------------------------------------------------------------------

// errorBody は全エンドポイント共通のエラー形式。
// code は画面側が分岐に使う識別子、message はそのまま表示できる日本語。
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// API の応答は状態が刻々と変わるので、ブラウザや中継サーバーにキャッシュさせない。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// ---------------------------------------------------------------------------
// ヘルスチェック
// ---------------------------------------------------------------------------

// handleHealthz は「プロセスが生きているか」だけを答える（Kubernetes の livenessProbe 用）。
// DB などの依存先は見ない。DB の一時的な障害でこのコンテナまで再起動されると、
// 実行中の復元ジョブが無駄に中断されるため。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

// handleReadyz は「リクエストを受けられる状態か」を答える（readinessProbe 用）。
// DB に繋がらない・保存先が読めないときは 503 を返し、Service の転送先から外してもらう。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.ready(r.Context()); err != nil {
		s.log.Warn("readyz が失敗", "error", err)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "依存先に接続できません。")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ready\n")
}

// ---------------------------------------------------------------------------
// 設定
// ---------------------------------------------------------------------------

type configResponse struct {
	Limits         limitsResponse `json:"limits"`
	RetentionHours float64        `json:"retentionHours"`
	// KeepCompleted が true なら、完成したスキャンはギャラリーに保存され、保存期限で消えない。
	KeepCompleted bool         `json:"keepCompleted"`
	Pipeline      PipelineInfo `json:"pipeline"`
}

type limitsResponse struct {
	MinImages      int   `json:"minImages"`
	MaxImages      int   `json:"maxImages"`
	MaxImageBytes  int64 `json:"maxImageBytes"`
	MaxUploadBytes int64 `json:"maxUploadBytes"`
	MaxImagePixels int64 `json:"maxImagePixels"`
	MinImageSide   int   `json:"minImageSide"`
}

// handleConfig は画面が最初に読む設定を返す。
//
// 上限値をサーバーから渡す理由: 画面側に同じ数字を書き写すと、片方だけ変えて食い違う。
// サーバーの設定を唯一の出どころにして、画面は送信前の案内（「あと3枚必要」など）に使う。
// 最終的な検証は必ずサーバー側でも行う（画面の検証は迂回できるため）。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	l := s.cfg.Limits
	writeJSON(w, http.StatusOK, configResponse{
		Limits: limitsResponse{
			MinImages: l.MinImages, MaxImages: l.MaxImages, MaxImageBytes: l.MaxImageBytes,
			MaxUploadBytes: l.MaxUploadBytes, MaxImagePixels: l.MaxImagePixels, MinImageSide: l.MinImageSide,
		},
		RetentionHours: s.cfg.Jobs.Retention.Hours(),
		KeepCompleted:  s.cfg.Jobs.KeepCompleted,
		Pipeline:       s.pipeline,
	})
}

// ---------------------------------------------------------------------------
// スキャンの作成
// ---------------------------------------------------------------------------

type createResponse struct {
	ID     string      `json:"id"`
	Status scan.Status `json:"status"`
	// FrameCount は処理に使う写真の枚数（続きの版では、引き継いだ写真を含む）。
	FrameCount int `json:"frameCount"`
	// BaseFrameCount は元の版から引き継いだ写真の枚数（新規なら 0）。
	BaseFrameCount    int       `json:"baseFrameCount"`
	DuplicatesSkipped int       `json:"duplicatesSkipped"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

// handleCreateScan は写真を受け取り、復元ジョブを待ち行列に入れる。
//
// 入力: multipart/form-data。項目 "images"（複数）と任意の "metadata"（JSON）。
// 出力: 202 Accepted と {id, status:"queued", ...}。
//
// なぜ 200 ではなく 202 なのか: 復元は数分〜数時間かかる。HTTP の応答をそれまで待たせると、
// ブラウザや途中の中継（Cloudflare は約2分で 524 を返す）が先に切ってしまう。
// 「受け付けた。結果は GET /api/scans/{id} で確認して」と即答するのが 202 の意味。
//
// 副作用: PVC に写真を保存し、DB に行を作り、ワーカーを起こす。
// 失敗時は保存しかけたファイルを必ず消す（半端なデータを残さない）。
func (s *Server) handleCreateScan(w http.ResponseWriter, r *http.Request) {
	s.createScan(w, r, nil)
}

// handleResumeScan は「続きを撮る」の写真を受け取り、続きの版を待ち行列に入れる
// （POST /api/scans/{id}/resume。本文は POST /api/scans と同じ形）。
//
// 続きの版は新しいスキャンとして作り、元の版の写真と地図を引き継ぐ（workspace.InheritFrom）。
// 元の版は書き換えないので、追加撮影がうまくいかなくても前の3Dデータはそのまま残る。
// 写真は1枚から受け付ける。上限は引き継いだ写真と合わせて MAX_IMAGES 枚まで
// （処理には全部の写真を使うので、処理時間とメモリは合計の枚数で決まる）。
func (s *Server) handleResumeScan(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if !s.resumable(d.Scan) {
		writeError(w, http.StatusConflict, "not_resumable", notResumableMessage)
		return
	}
	parent := d.Scan
	s.createScan(w, r, &parent)
}

// notResumableMessage は「続きを撮る」と位置合わせができないスキャンへの説明。
const notResumableMessage = "このスキャンには続きを撮れません。完成していて、位置合わせ用の地図（カメラ位置と特徴点）が残っているものだけが対象です。"

// resumable は、スキャンが「続きを撮る」と位置合わせ（VPS）に使えるかを返す。
// 完成していて（見られる3Dデータがある）、地図のファイルが残っているもの。
func (s *Server) resumable(sc scan.Scan) bool {
	if !sc.Status.Keepable() {
		return false
	}
	ws, err := s.root.Scan(sc.ID)
	return err == nil && ws.HasMap()
}

// createScan は新規と「続きを撮る」に共通の受付処理。parent が nil なら新規。
func (s *Server) createScan(w http.ResponseWriter, r *http.Request, parent *scan.Scan) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "multipart/form-data" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "multipart/form-data で送信してください。")
		return
	}

	// 同時アップロード数の制限。席が空いていなければ待たせずに断る。
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "busy", "他のアップロードを処理中です。少し待ってからもう一度お試しください。")
		return
	}

	// 受け付ける前に「いま受けてよい状態か」を確かめる。大きなボディを読み終えてから断るより、
	// 読む前に断る方が、利用者の通信量もサーバーのディスクも無駄にしない。
	active, err := s.store.CountActive(r.Context())
	if err != nil {
		s.log.Error("待ち行列を確認できません", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
		return
	}
	if active >= s.cfg.Limits.MaxQueuedScans {
		w.Header().Set("Retry-After", "600")
		writeError(w, http.StatusTooManyRequests, "queue_full",
			fmt.Sprintf("処理待ちが%d件あり、これ以上受け付けられません。しばらく待ってからお試しください。", active))
		return
	}
	free, err := s.root.FreeBytes()
	if err != nil {
		s.log.Error("空き容量を確認できません", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
		return
	}
	// 「最低限残しておく空き」＋「今回のアップロードで増えうる量」が無ければ断る。
	// 続きの版は、処理の始めに元の版の COLMAP DB を複製するので、その大きさも見込む。
	need := s.cfg.Limits.MinFreeDiskBytes + s.cfg.Limits.MaxUploadBytes
	var parentWS *workspace.Scan
	if parent != nil {
		if parentWS, err = s.root.Scan(parent.ID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
			return
		}
		if fi, err := os.Stat(parentWS.DatabasePath()); err == nil {
			need += fi.Size()
		}
	}
	if free < need {
		writeError(w, http.StatusInsufficientStorage, "disk_full", "サーバーの保存領域が不足しています。時間をおいてお試しください。")
		return
	}

	id, err := scan.NewID()
	if err != nil {
		s.log.Error("ID を作れません", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
		return
	}
	ws, err := s.root.Scan(id)
	if err == nil {
		err = ws.Create()
	}
	if err != nil {
		s.log.Error("作業ディレクトリを作れません", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
		return
	}
	// ここから先で失敗したら、作りかけのディレクトリを消す。
	created := false
	defer func() {
		if !created {
			if err := ws.Remove(); err != nil {
				s.log.Error("作りかけの作業ディレクトリを消せません", "scan", id, "error", err)
			}
		}
	}()

	// 続きの版: 元の版の写真と地図を引き継ぎ、今回の写真はその続きの番号で保存する。
	var inherited workspace.Inherited
	opts := upload.Options{}
	if parent != nil {
		if inherited, err = ws.InheritFrom(parentWS); err != nil {
			s.log.Error("元の版を引き継げません", "scan", id, "parent", parent.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "元の3Dデータを引き継げませんでした。")
			return
		}
		room := s.cfg.Limits.MaxImages - inherited.Frames
		if room < 1 {
			writeError(w, http.StatusBadRequest, "too_many_images",
				fmt.Sprintf("これ以上写真を追加できません（元の写真と合わせて%d枚までです）。", s.cfg.Limits.MaxImages))
			return
		}
		opts = upload.Options{FirstNumber: inherited.NextNumber, MinImages: 1, MaxImages: room}
	}

	// リクエスト全体の上限。これを超えて読もうとすると読み取りがエラーになる
	// （Content-Length を偽ったり省略したりしても効く）。
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Limits.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_upload", "アップロードの形式が正しくありません。")
		return
	}
	res, err := upload.ReceiveWith(mr, ws.ImagesDir(), s.cfg.Limits, opts)
	if err != nil {
		var ue *upload.Error
		if errors.As(err, &ue) {
			writeError(w, ue.Status, ue.Code, ue.Message)
			return
		}
		s.log.Error("写真を保存できません", "scan", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "写真を保存できませんでした。")
		return
	}

	// 撮影側が写真の EXIF から読んだ焦点距離があれば、位置推定の手掛かりとして残す（reconstruction.CameraHint）。
	if focal, ok := upload.FocalHint(res.Frames); ok {
		hint := reconstruction.CameraHint{FocalLength35mm: focal, Width: res.Frames[0].Width, Height: res.Frames[0].Height}
		if err := reconstruction.WriteCameraHint(ws, hint); err != nil {
			s.log.Warn("焦点距離の手掛かりを保存できません", "scan", id, "error", err)
		}
	}

	// ギャラリー用の縮小画像を、1枚目の写真から作っておく。失敗しても受付は続ける（一覧で画像が出ないだけ）。
	// 続きの版は元の版の縮小画像を引き継いでいる（一覧で同じ見た目にする）ので、無いときだけ作る。
	if _, err := os.Stat(ws.ThumbnailPath()); err != nil && len(res.Frames) > 0 {
		if err := thumbnail.Make(filepath.Join(ws.ImagesDir(), res.Frames[0].FileName), ws.ThumbnailPath(), thumbnailSide); err != nil {
			s.log.Warn("サムネイルを作れません", "scan", id, "error", err)
		}
	}

	now := s.now().UTC()
	sc := scan.Scan{
		ID:             id,
		CreatedAt:      now,
		ExpiresAt:      now.Add(s.cfg.Jobs.Retention),
		FrameCount:     inherited.Frames + len(res.Frames),
		BaseFrameCount: inherited.Frames,
		CaptureMeta:    res.ClientMeta,
	}
	if parent != nil {
		// 名前は引き継ぐ（ギャラリーで同じものの新しい版だと分かるように）。
		sc.ParentID, sc.Title = parent.ID, parent.Title
	}
	if err := s.store.Create(r.Context(), sc, res.Frames); err != nil {
		s.log.Error("スキャンを登録できません", "scan", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "スキャンを登録できませんでした。")
		return
	}
	created = true
	s.jobs.Notify()

	s.log.Info("スキャンを受け付けました", "scan", id, "frames", len(res.Frames), "inherited", inherited.Frames,
		"parent", sc.ParentID, "duplicates", res.DuplicatesSkipped)
	w.Header().Set("Location", "/api/scans/"+id)
	writeJSON(w, http.StatusAccepted, createResponse{
		ID: id, Status: scan.StatusQueued, FrameCount: sc.FrameCount, BaseFrameCount: sc.BaseFrameCount,
		DuplicatesSkipped: res.DuplicatesSkipped, ExpiresAt: sc.ExpiresAt,
	})
}

// ---------------------------------------------------------------------------
// スキャンの取得
// ---------------------------------------------------------------------------

type scanResponse struct {
	ID         string      `json:"id"`
	Status     scan.Status `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	UpdatedAt  time.Time   `json:"updatedAt"`
	ExpiresAt  time.Time   `json:"expiresAt"`
	StartedAt  *time.Time  `json:"startedAt"`
	FinishedAt *time.Time  `json:"finishedAt"`
	FrameCount int         `json:"frameCount"`
	// Saved はギャラリーに保存されているか（true なら期限で消えない）。
	Saved bool   `json:"saved"`
	Title string `json:"title"`
	// ThumbnailURL は縮小画像の URL。無ければ省略。
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
	// ParentID は元の版（「続きを撮る」で作った場合）。新規、または元の版が削除済みなら省略。
	ParentID string `json:"parentId,omitempty"`
	// BaseFrameCount は元の版から引き継いだ写真の枚数（frameCount に含まれる）。
	BaseFrameCount int `json:"baseFrameCount"`
	// Resumable は「続きを撮る」と位置合わせ（VPS）に使えるか。
	Resumable bool `json:"resumable"`
	// Continuations はこの版を元にした続きの版（新しい順）。
	Continuations []continuationResponse `json:"continuations"`
	// SplatJob は、あとで作る 3DGS が待ち行列にあればその状況（無ければ省略）。
	SplatJob *splatJobResponse `json:"splatJob,omitempty"`
	// QueuePosition は自分より前に並んでいる件数（queued のときだけ入る）。
	QueuePosition *int            `json:"queuePosition,omitempty"`
	Error         *errorDetail    `json:"error"`
	Steps         []stepResponse  `json:"steps"`
	Assets        []assetResponse `json:"assets"`
}

// splatJobResponse は、あとで作る 3DGS の状況。
type splatJobResponse struct {
	// Status は "queued"（順番待ち）か "running"（学習中）。
	Status scan.SplatJobStatus `json:"status"`
	// QueuePosition は自分より前に並んでいる 3DGS の件数（順番待ちのときだけ入る）。
	QueuePosition *int `json:"queuePosition,omitempty"`
	// Attempts は着手した回数（新しいスキャンに順番を譲った回は数えない）。
	Attempts int `json:"attempts"`
}

// continuationResponse は続きの版1件の概要。
type continuationResponse struct {
	ID         string      `json:"id"`
	Status     scan.Status `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	FrameCount int         `json:"frameCount"`
}

type stepResponse struct {
	Name       scan.StepName   `json:"name"`
	Status     scan.StepStatus `json:"status"`
	StartedAt  *time.Time      `json:"startedAt"`
	FinishedAt *time.Time      `json:"finishedAt"`
	Message    string          `json:"message"`
	Error      string          `json:"error"`
	// HasLog は処理ログを取得できるかどうか。
	HasLog bool `json:"hasLog"`
	// Progress は実行中ステップの実測の進み具合。分からないステップでは省略される。
	Progress *stepProgress `json:"progress,omitempty"`
}

type assetResponse struct {
	Kind      scan.AssetKind `json:"kind"`
	SizeBytes int64          `json:"sizeBytes"`
	URL       string         `json:"url"`
}

// lookup は URL の {id} を検証し、スキャンを読む。失敗したら応答を書いて false を返す。
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (scan.Detail, bool) {
	id := r.PathValue("id")
	// 形式の検査が最初。ここを通った ID には、パスに意味を持つ文字が含まれない。
	if !scan.ValidID(id) {
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
		return scan.Detail{}, false
	}
	d, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。保存期限を過ぎて削除された可能性があります。")
		return scan.Detail{}, false
	}
	if err != nil {
		s.log.Error("スキャンを読めません", "scan", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
		return scan.Detail{}, false
	}
	return d, true
}

func (s *Server) handleGetScan(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	sc := d.Scan
	resp := scanResponse{
		ID: sc.ID, Status: sc.Status, CreatedAt: sc.CreatedAt, UpdatedAt: sc.UpdatedAt, ExpiresAt: sc.ExpiresAt,
		StartedAt: sc.StartedAt, FinishedAt: sc.FinishedAt, FrameCount: sc.FrameCount,
		Saved: sc.Saved, Title: sc.Title, ThumbnailURL: s.thumbnailURL(sc),
		ParentID: sc.ParentID, BaseFrameCount: sc.BaseFrameCount, Resumable: s.resumable(sc),
		Continuations: []continuationResponse{},
		Steps:         []stepResponse{}, Assets: []assetResponse{},
	}
	if d.Splat != nil {
		resp.SplatJob = &splatJobResponse{Status: d.Splat.Status, Attempts: d.Splat.Attempts}
		if d.Splat.Status == scan.SplatQueued {
			if pos, err := s.store.SplatQueuePosition(r.Context(), sc.ID); err == nil {
				resp.SplatJob.QueuePosition = &pos
			}
		}
	}
	if children, err := s.store.Children(r.Context(), sc.ID); err == nil {
		for _, c := range children {
			resp.Continuations = append(resp.Continuations, continuationResponse{
				ID: c.ID, Status: c.Status, CreatedAt: c.CreatedAt, FrameCount: c.FrameCount,
			})
		}
	} else {
		s.log.Warn("続きの版を読めません", "scan", sc.ID, "error", err)
	}
	if sc.ErrorCode != "" || sc.ErrorMessage != "" {
		resp.Error = &errorDetail{Code: sc.ErrorCode, Message: sc.ErrorMessage}
	}
	if sc.Status == scan.StatusQueued {
		if pos, err := s.store.QueuePosition(r.Context(), sc.ID); err == nil {
			resp.QueuePosition = &pos
		}
	}
	ws, wsErr := s.root.Scan(sc.ID)
	for _, st := range d.Steps {
		sr := stepResponse{Name: st.Name, Status: st.Status, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt,
			Message: st.Message, Error: st.ErrorMessage}
		if wsErr == nil {
			if fi, err := os.Stat(ws.LogPath(st.Name)); err == nil && fi.Size() > 0 {
				sr.HasLog = true
			}
			if st.Name == scan.StepSplat && st.Status == scan.StepRunning {
				sr.Progress = splatProgress(ws.LogPath(st.Name), s.pipeline.SplatIterations)
			}
		}
		resp.Steps = append(resp.Steps, sr)
	}
	for _, a := range d.Assets {
		// URL の末尾にファイル名（拡張子つき）を付けて返す。ビューアが形式を見分けやすい。
		resp.Assets = append(resp.Assets, assetResponse{
			Kind: a.Kind, SizeBytes: a.SizeBytes,
			URL: "/api/scans/" + sc.ID + "/assets/" + string(a.Kind) + "/" + scan.Assets[a.Kind].DownloadName,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// maxLogBytes は1回の応答で返すログの最大量（末尾から）。
const maxLogBytes = 256 * 1024

// handleGetLogs はステップの処理ログの末尾を返す。
func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	// step は「知っているステップ名」と突き合わせる。
	// 受け取った文字列をそのままファイル名にしない（?step=../../etc/passwd のような入力を防ぐ）。
	var step scan.StepName
	for _, name := range scan.StepOrder {
		if string(name) == r.URL.Query().Get("step") {
			step = name
		}
	}
	if step == "" {
		writeError(w, http.StatusBadRequest, "bad_step", "step には sfm / mvs / mesh / splat / sog のいずれかを指定してください。")
		return
	}
	ws, err := s.root.Scan(d.Scan.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "ログがありません。")
		return
	}
	f, err := os.Open(ws.LogPath(step))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_log", "このステップのログはまだありません。")
		return
	}
	defer f.Close()

	// ログは数MBになりうるので、末尾だけを返す。
	if fi, err := f.Stat(); err == nil && fi.Size() > maxLogBytes {
		if _, err := f.Seek(-maxLogBytes, io.SeekEnd); err == nil {
			w.Header().Set("X-Log-Truncated", "true")
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, f)
}

// handleGetAsset は成果物ファイルを返す。
//
// 守っていること:
//   - protect（同一オリジン確認）を通ったリクエストだけが来る。
//   - {kind} は「知っている種類」の表で検証する。ファイル名を URL から受け取らない。
//   - 実際のパスは DB の相対パスから求め、作業ディレクトリの外を指していないか確かめる。
//   - 処理待ち・中止・期限切れのスキャンからは配信しない。
func (s *Server) handleGetAsset(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	kind := scan.AssetKind(r.PathValue("kind"))
	info, known := scan.Assets[kind]
	if !known {
		writeError(w, http.StatusNotFound, "unknown_asset", "その種類の成果物はありません。")
		return
	}
	// ファイル名つきの URL では、名前が「その種類の決まった名前」と一致することだけを確かめる。
	if name := r.PathValue("name"); name != "" && name != info.DownloadName {
		writeError(w, http.StatusNotFound, "unknown_asset", "その名前の成果物はありません。")
		return
	}
	switch d.Scan.Status {
	case scan.StatusCancelled, scan.StatusExpired, scan.StatusQueued:
		writeError(w, http.StatusGone, "asset_unavailable", "このスキャンの成果物は利用できません（中止・期限切れ・未処理）。")
		return
	}
	var asset *scan.Asset
	for i := range d.Assets {
		if d.Assets[i].Kind == kind {
			asset = &d.Assets[i]
		}
	}
	if asset == nil {
		writeError(w, http.StatusNotFound, "asset_not_ready", "この成果物はまだ生成されていないか、生成に失敗しました。")
		return
	}
	ws, err := s.root.Scan(d.Scan.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "成果物が見つかりません。")
		return
	}
	path, err := ws.Resolve(asset.RelativePath)
	if err != nil {
		// DB の値が作業ディレクトリの外を指している。通常は起きないので、起きたら必ず記録する。
		s.log.Error("成果物のパスが不正です", "scan", d.Scan.ID, "kind", kind, "error", err)
		writeError(w, http.StatusNotFound, "not_found", "成果物が見つかりません。")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "成果物のファイルが見つかりません。")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not_found", "成果物のファイルが見つかりません。")
		return
	}

	w.Header().Set("Content-Type", info.ContentType)
	// 3DGS は作り直すと同じ URL のまま中身が変わるので、使うたびにブラウザに確かめさせる（no-cache）。
	// 変わっていなければ ServeContent が更新時刻を見て 304 を返すので、ファイル本体は送り直さない。
	// private: 途中の共有キャッシュには保存させない。
	w.Header().Set("Cache-Control", "private, no-cache")
	if dl, _ := strconv.ParseBool(r.URL.Query().Get("download")); dl {
		// attachment を付けると、ブラウザは表示せずファイルとして保存する。
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.DownloadName}))
	}
	// ServeContent はファイルを少しずつ送り（全体をメモリに載せない）、
	// Range リクエスト（途中からの再開）にも対応する。
	http.ServeContent(w, r, info.DownloadName, fi.ModTime(), f)
}

// ---------------------------------------------------------------------------
// ギャラリー（一覧・サムネイル・名前の変更）
// ---------------------------------------------------------------------------

// thumbnailSide はサムネイルの長辺（画素）。一覧のカードは幅 200 前後なので、高精細な画面でも足りる大きさ。
const thumbnailSide = 480

// thumbnailURL はサムネイルがあればその URL を、無ければ空文字を返す。
func (s *Server) thumbnailURL(sc scan.Scan) string {
	if sc.Status == scan.StatusExpired || sc.Status == scan.StatusCancelled {
		return ""
	}
	ws, err := s.root.Scan(sc.ID)
	if err != nil {
		return ""
	}
	if fi, err := os.Stat(ws.ThumbnailPath()); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return "/api/scans/" + sc.ID + "/thumbnail"
}

type galleryItem struct {
	ID         string      `json:"id"`
	Status     scan.Status `json:"status"`
	Title      string      `json:"title"`
	CreatedAt  time.Time   `json:"createdAt"`
	FinishedAt *time.Time  `json:"finishedAt"`
	ExpiresAt  time.Time   `json:"expiresAt"`
	Saved      bool        `json:"saved"`
	FrameCount int         `json:"frameCount"`
	// AssetKinds は登録済みの成果物の種類（メッシュ・3DGS があるかを一覧で示すため）。
	AssetKinds   []scan.AssetKind `json:"assetKinds"`
	ThumbnailURL string           `json:"thumbnailUrl,omitempty"`
	// ParentID は元の版（「続きを撮る」で作った場合）。BaseFrameCount は引き継いだ写真の枚数。
	ParentID       string `json:"parentId,omitempty"`
	BaseFrameCount int    `json:"baseFrameCount"`
	// SplatPending は 3DGS をあとで作る予定（順番待ちか学習中）かどうか。
	SplatPending bool `json:"splatPending"`
}

type galleryResponse struct {
	Items []galleryItem `json:"items"`
	// NextCursor は続きを読むときに ?after= に渡す値。続きが無ければ省略。
	NextCursor string `json:"nextCursor,omitempty"`
}

const (
	galleryDefaultLimit = 24
	galleryMaxLimit     = 60
)

// handleListScans はギャラリーの一覧を新しい順に返す（GET /api/scans）。
// 期限切れのものは含めない。処理中・失敗したものも含める（あとから進み具合や理由を確かめられるように）。
func (s *Server) handleListScans(w http.ResponseWriter, r *http.Request) {
	q := store.ListQuery{Limit: galleryDefaultLimit}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > galleryMaxLimit {
			writeError(w, http.StatusBadRequest, "bad_limit", fmt.Sprintf("limit は 1〜%d で指定してください。", galleryMaxLimit))
			return
		}
		q.Limit = n
	}
	if v := r.URL.Query().Get("after"); v != "" {
		c, ok := parseCursor(v)
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_cursor", "after の値が正しくありません。")
			return
		}
		q.After = &c
	}
	// 1件多く読み、あふれたら「続きがある」と分かる（件数を数える問い合わせを別にしない）。
	q.Limit++
	rows, err := s.store.List(r.Context(), q)
	if err != nil {
		s.log.Error("一覧を読めません", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
		return
	}
	resp := galleryResponse{Items: []galleryItem{}}
	limit := q.Limit - 1
	for i, row := range rows {
		if i == limit {
			last := rows[limit-1].Scan
			resp.NextCursor = formatCursor(store.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
			break
		}
		sc := row.Scan
		kinds := row.AssetKinds
		if kinds == nil {
			kinds = []scan.AssetKind{}
		}
		resp.Items = append(resp.Items, galleryItem{
			ID: sc.ID, Status: sc.Status, Title: sc.Title, CreatedAt: sc.CreatedAt, FinishedAt: sc.FinishedAt,
			ExpiresAt: sc.ExpiresAt, Saved: sc.Saved, FrameCount: sc.FrameCount, AssetKinds: kinds,
			ThumbnailURL: s.thumbnailURL(sc), ParentID: sc.ParentID, BaseFrameCount: sc.BaseFrameCount,
			SplatPending: row.SplatPending,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// カーソルは「作成時刻（UNIX ナノ秒）_ID」という文字列にする。
// 中身を画面が解釈する必要は無く、そのまま次の要求に付けて返すだけ。
func formatCursor(c store.Cursor) string {
	return strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + "_" + c.ID
}

func parseCursor(v string) (store.Cursor, bool) {
	ts, id, ok := strings.Cut(v, "_")
	if !ok || !scan.ValidID(id) {
		return store.Cursor{}, false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return store.Cursor{}, false
	}
	return store.Cursor{CreatedAt: time.Unix(0, n).UTC(), ID: id}, true
}

// handleGetThumbnail はサムネイル（JPEG）を返す。
func (s *Server) handleGetThumbnail(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if d.Scan.Status == scan.StatusExpired || d.Scan.Status == scan.StatusCancelled {
		writeError(w, http.StatusGone, "thumbnail_unavailable", "このスキャンの写真は削除されています。")
		return
	}
	ws, err := s.root.Scan(d.Scan.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "サムネイルがありません。")
		return
	}
	f, err := os.Open(ws.ThumbnailPath())
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "サムネイルがありません。")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not_found", "サムネイルがありません。")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// 一度作ったら変わらない。一覧を開くたびに取り直さないよう、ブラウザ内に1日キャッシュさせる。
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "thumbnail.jpg", fi.ModTime(), f)
}

// handleUpdateScan はスキャンの名前を変える（PATCH /api/scans/{id}、本文は {"title": "..."}）。
func (s *Server) handleUpdateScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !scan.ValidID(id) {
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
		return
	}
	var body struct {
		Title *string `json:"title"`
	}
	// 名前1つを送るだけの小さな本文。巨大な JSON を読まされないよう上限を付ける。
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Title == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "title を JSON で指定してください。")
		return
	}
	title, ok := cleanTitle(*body.Title)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_title", fmt.Sprintf("名前は%d文字以内で、改行などの制御文字を含めないでください。", scan.MaxTitleLength))
		return
	}
	err := s.store.SetTitle(r.Context(), id, title, s.now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
		return
	}
	if err != nil {
		s.log.Error("名前を変更できません", "scan", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "名前を変更できませんでした。")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"title": title})
}

// cleanTitle は名前の前後の空白を除き、長さと文字を検証する。空文字は「名前なし」として受け付ける。
func cleanTitle(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > scan.MaxTitleLength {
		return "", false
	}
	for _, c := range v {
		if unicode.IsControl(c) {
			return "", false
		}
	}
	return v, true
}

// ---------------------------------------------------------------------------
// 位置合わせ（VPS）
// ---------------------------------------------------------------------------

// Localizer は写真1枚を地図に位置合わせする（本番では *vps.Localizer、テストでは偽物）。
type Localizer interface {
	Localize(ctx context.Context, m *workspace.Scan, q vps.Query) (vps.Result, error)
}

// localizeResponse は位置合わせの結果。座標は、そのスキャンの地図（COLMAP が決めた座標系）で表す。
// scene.json やダウンロードしたファイルと同じ座標系なので、画面はそのまま重ねて描ける。
type localizeResponse struct {
	Localized bool `json:"localized"`
	// Position はカメラの位置、Forward はレンズの向き（単位ベクトル）。位置が分からなければ省略。
	Position *[3]float64 `json:"position,omitempty"`
	Forward  *[3]float64 `json:"forward,omitempty"`
	// Matches は写真の特徴点のうち地図の3D点に対応づいた数（多いほど確か）。
	Matches        int   `json:"matches"`
	ComparedImages int   `json:"comparedImages"`
	MapImages      int   `json:"mapImages"`
	DurationMs     int64 `json:"durationMs"`
	// Message はそのまま画面に出せる説明。
	Message string `json:"message"`
}

// handleLocalize は写真1枚を受け取り、スキャンの地図のどこから撮ったものかを返す
// （POST /api/scans/{id}/localize。multipart/form-data の項目 "image" に写真1枚）。
//
// 処理には10〜数十秒かかる（COLMAP で特徴抽出・マッチング・登録を行う）。202 で受けて後から
// 問い合わせる形にしないのは、結果を残す必要がなく、1枚ぶんなら前段の時間制限（約100秒）に収まるため。
// 制限時間（vps.Localizer.Timeout）を超えたら 504 で打ち切る。
func (s *Server) handleLocalize(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if !s.resumable(d.Scan) {
		writeError(w, http.StatusConflict, "not_resumable", notResumableMessage)
		return
	}
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "multipart/form-data" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "multipart/form-data で送信してください。")
		return
	}
	tmp, err := s.root.NewTemp("query-")
	if err != nil {
		s.log.Error("一時ディレクトリを作れません", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
		return
	}
	defer os.RemoveAll(tmp)

	// 写真1枚ぶん（＋multipart の区切りなど）より大きな本文は読まない。
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Limits.MaxImageBytes+64*1024)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_upload", "アップロードの形式が正しくありません。")
		return
	}
	res, err := upload.ReceiveWith(mr, tmp, s.cfg.Limits, upload.Options{MinImages: 1, MaxImages: 1})
	if err != nil {
		var ue *upload.Error
		if errors.As(err, &ue) {
			writeError(w, ue.Status, ue.Code, ue.Message)
			return
		}
		s.log.Error("写真を保存できません", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "写真を受け取れませんでした。")
		return
	}
	frame := res.Frames[0]
	ws, err := s.root.Scan(d.Scan.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
		return
	}

	start := time.Now()
	out, err := s.localizer.Localize(r.Context(), ws, vps.Query{
		Path: filepath.Join(tmp, frame.FileName), Width: frame.Width, Height: frame.Height,
	})
	elapsed := time.Since(start)
	switch {
	case errors.Is(err, vps.ErrBusy):
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, "busy", "別の位置合わせを処理中です。数秒待ってからもう一度お試しください。")
		return
	case errors.Is(err, vps.ErrTimeout):
		writeError(w, http.StatusGatewayTimeout, "localize_timeout",
			"時間内に位置を確かめられませんでした。3Dデータを作っている間は遅くなります。少し待ってからお試しください。")
		return
	case err != nil:
		if r.Context().Err() != nil {
			return // 利用者が待つのをやめた。応答を受け取る相手が居ない。
		}
		s.log.Error("位置合わせに失敗", "scan", d.Scan.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "localize_failed", "位置合わせの処理に失敗しました。")
		return
	}

	resp := localizeResponse{
		Localized: out.Localized, Matches: out.Matches, ComparedImages: out.ComparedImages,
		MapImages: out.MapImages, DurationMs: elapsed.Milliseconds(),
	}
	if out.Localized {
		resp.Position, resp.Forward = &out.Position, &out.Forward
		resp.Message = fmt.Sprintf("位置が分かりました（地図の3D点%d個と一致）。", out.Matches)
	} else {
		resp.Message = "保存した3Dデータと一致する景色が見つかりませんでした。前回撮った部分が写るように向けて、もう一度お試しください。"
	}
	s.log.Info("位置合わせ", "scan", d.Scan.ID, "localized", out.Localized, "matches", out.Matches,
		"compared", out.ComparedImages, "duration", elapsed.Round(time.Millisecond))
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// 3DGS の作り直し
// ---------------------------------------------------------------------------

// handleMakeSplat は、完成したスキャンの 3DGS を待ち行列に入れる（POST /api/scans/{id}/splat）。
//
// 使いどころ: GPU を使えるようになった後や、反復回数（SPLAT_ITERATIONS）を増やした後に、
// 保存済みのスキャンの 3DGS を作り直す。前の 3DGS は、新しいものが出来るまで見られるまま残る。
// 学習は位置推定の結果（images/ と sparse/0/）から行うので、地図が残っているスキャンだけが対象。
func (s *Server) handleMakeSplat(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if !s.cfg.Pipeline.EnableSplat {
		writeError(w, http.StatusConflict, "splat_disabled", "このサーバーでは 3DGS の生成が無効になっています。")
		return
	}
	if !s.resumable(d.Scan) {
		writeError(w, http.StatusConflict, "not_ready", "3DGS を作れるのは、完成していて位置推定の結果が残っているスキャンだけです。")
		return
	}
	err := s.jobs.EnqueueSplat(r.Context(), d.Scan.ID)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "splat_busy", "3DGS はすでに作成待ちか作成中です。")
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
		return
	case err != nil:
		s.log.Error("3DGS を待ち行列に入れられません", "scan", d.Scan.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "3DGS の作成を受け付けられませんでした。")
		return
	}
	s.log.Info("3DGS の作成を受け付けました", "scan", d.Scan.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": string(scan.SplatQueued)})
}

// ---------------------------------------------------------------------------
// キャンセル／削除
// ---------------------------------------------------------------------------

func (s *Server) handleDeleteScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !scan.ValidID(id) {
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
		return
	}
	result, err := s.jobs.Remove(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "スキャンが見つかりません。")
		return
	}
	if err != nil {
		s.log.Error("スキャンを削除できません", "scan", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "削除できませんでした。")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": string(result)})
}
