package httpapi

import (
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

	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/thumbnail"
	"github.com/ieyoukan/3d-scanner/server/internal/upload"
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
	ID                string      `json:"id"`
	Status            scan.Status `json:"status"`
	FrameCount        int         `json:"frameCount"`
	DuplicatesSkipped int         `json:"duplicatesSkipped"`
	ExpiresAt         time.Time   `json:"expiresAt"`
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
	if free < s.cfg.Limits.MinFreeDiskBytes+s.cfg.Limits.MaxUploadBytes {
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

	// リクエスト全体の上限。これを超えて読もうとすると読み取りがエラーになる
	// （Content-Length を偽ったり省略したりしても効く）。
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Limits.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_upload", "アップロードの形式が正しくありません。")
		return
	}
	res, err := upload.Receive(mr, ws.ImagesDir(), s.cfg.Limits)
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

	// ギャラリー用の縮小画像を、1枚目の写真から作っておく。失敗しても受付は続ける（一覧で画像が出ないだけ）。
	if len(res.Frames) > 0 {
		if err := thumbnail.Make(filepath.Join(ws.ImagesDir(), res.Frames[0].FileName), ws.ThumbnailPath(), thumbnailSide); err != nil {
			s.log.Warn("サムネイルを作れません", "scan", id, "error", err)
		}
	}

	now := s.now().UTC()
	sc := scan.Scan{
		ID:          id,
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.cfg.Jobs.Retention),
		FrameCount:  len(res.Frames),
		CaptureMeta: res.ClientMeta,
	}
	if err := s.store.Create(r.Context(), sc, res.Frames); err != nil {
		s.log.Error("スキャンを登録できません", "scan", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "スキャンを登録できませんでした。")
		return
	}
	created = true
	s.jobs.Notify()

	s.log.Info("スキャンを受け付けました", "scan", id, "frames", len(res.Frames), "duplicates", res.DuplicatesSkipped)
	w.Header().Set("Location", "/api/scans/"+id)
	writeJSON(w, http.StatusAccepted, createResponse{
		ID: id, Status: scan.StatusQueued, FrameCount: len(res.Frames),
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
	// QueuePosition は自分より前に並んでいる件数（queued のときだけ入る）。
	QueuePosition *int            `json:"queuePosition,omitempty"`
	Error         *errorDetail    `json:"error"`
	Steps         []stepResponse  `json:"steps"`
	Assets        []assetResponse `json:"assets"`
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
		Steps: []stepResponse{}, Assets: []assetResponse{},
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
	// 成果物は一度出来たら変わらないので、利用者のブラウザ内に限って短時間キャッシュさせる
	// （private: 途中の共有キャッシュには保存させない）。タブを切り替えるたびの再取得を減らす。
	w.Header().Set("Cache-Control", "private, max-age=600")
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
			ThumbnailURL: s.thumbnailURL(sc),
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
