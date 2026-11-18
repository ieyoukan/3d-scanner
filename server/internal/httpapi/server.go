// Package httpapi は HTTP の入口（ハンドラ層）。
//
// 役割は「リクエストを検証して、適切な層に渡し、結果を JSON で返す」ことだけ。
// 復元処理そのものは jobs / reconstruction パッケージが行う。
//
// エンドポイント一覧:
//
//	GET    /healthz                       生存確認（依存先を見ない）
//	GET    /readyz                        準備完了の確認（DB と保存先を見る）
//	GET    /api/config                    画面が使う設定（上限など）。認証不要
//	GET    /api/scans                     ギャラリーの一覧（新しい順。?limit= と ?after= で続きを読む）
//	POST   /api/scans                     写真をアップロードして復元ジョブを登録（202 Accepted）
//	GET    /api/scans/{id}                状態・ステップ・成果物の一覧
//	GET    /api/scans/{id}/logs?step=sfm  ステップの処理ログ（テキスト）
//	GET    /api/scans/{id}/assets/{kind}  成果物のダウンロード（末尾に /scene.sog 等のファイル名を付けてもよい）
//	GET    /api/scans/{id}/thumbnail      ギャラリー用の縮小画像（1枚目の写真から作る）
//	PATCH  /api/scans/{id}                名前の変更（{"title": "..."}）
//	DELETE /api/scans/{id}                キャンセル／削除
//	それ以外                               ビルド済み Web（SPA）を返す
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/config"
	"github.com/ieyoukan/3d-scanner/server/internal/jobs"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// PipelineInfo は画面に伝える「このサーバーで何が出来るか」。
type PipelineInfo struct {
	MeshEnabled bool `json:"meshEnabled"`
	// MeshMode は "dense"（密な復元あり）か "sparse"（疎な点群からの簡易メッシュ）。
	MeshMode        string `json:"meshMode"`
	SplatEnabled    bool   `json:"splatEnabled"`
	SplatIterations int    `json:"splatIterations"`
	// SplatDevice は "cpu" か "gpu"。
	SplatDevice   string `json:"splatDevice"`
	ColmapVersion string `json:"colmapVersion"`
	ColmapGPU     bool   `json:"colmapGpu"`
}

// Server は HTTP ハンドラ群と、その依存先をまとめたもの。
type Server struct {
	cfg      config.Config
	store    store.Store
	root     *workspace.Root
	jobs     *jobs.Service
	pipeline PipelineInfo
	log      *slog.Logger
	now      func() time.Time

	// uploadSlots は同時に受け付けるアップロードの数を制限する（セマフォ）。
	// 容量2のチャネルに「席札」を入れられた人だけが進める。席が無ければ 429 で断る。
	// 同時アップロードの数だけディスクと帯域を使うので、際限なく並行させない。
	uploadSlots chan struct{}
}

// New は Server を作る。
func New(cfg config.Config, st store.Store, root *workspace.Root, js *jobs.Service, info PipelineInfo, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg: cfg, store: st, root: root, jobs: js, pipeline: info, log: log,
		now:         time.Now,
		uploadSlots: make(chan struct{}, 2),
	}
}

// Handler は全ルートを登録した http.Handler を返す。
//
// Go 1.22 以降の標準 ServeMux は "GET /api/scans/{id}" のように
// メソッドとパスの変数を書けるので、外部のルーターライブラリは使っていない。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// アプリ自身は認証を持たない。アクセス制限は前段（Cloudflare Access など）で行う。
	// s.protect は、状態を変えるリクエストが自分のページから来たものかだけを確かめる。
	mux.HandleFunc("GET /api/config", s.handleConfig)

	mux.Handle("GET /api/scans", s.protect(s.handleListScans))
	mux.Handle("POST /api/scans", s.protect(s.handleCreateScan))
	mux.Handle("GET /api/scans/{id}", s.protect(s.handleGetScan))
	mux.Handle("GET /api/scans/{id}/logs", s.protect(s.handleGetLogs))
	mux.Handle("GET /api/scans/{id}/assets/{kind}", s.protect(s.handleGetAsset))
	// 末尾にファイル名を付けた別名（例: .../assets/gaussian_sog/scene.sog）。
	// 3Dライブラリには URL の拡張子で形式を見分けるものがあるため。{name} は種類ごとに決まった
	// 名前と一致するときだけ受け付け、ファイルの場所を決めるのには使わない。
	mux.Handle("GET /api/scans/{id}/assets/{kind}/{name}", s.protect(s.handleGetAsset))
	mux.Handle("GET /api/scans/{id}/thumbnail", s.protect(s.handleGetThumbnail))
	mux.Handle("PATCH /api/scans/{id}", s.protect(s.handleUpdateScan))
	mux.Handle("DELETE /api/scans/{id}", s.protect(s.handleDeleteScan))

	// 上のどれにも当たらない /api/... は JSON の 404 を返す（SPA の HTML を返さない）。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "そのAPIはありません。")
	})

	// それ以外は Web（SPA）。
	mux.Handle("/", s.staticHandler())

	// ミドルウェアは外側から順に適用される: ログ → パニック回復 → セキュリティヘッダ → 各ハンドラ。
	return s.logRequests(s.recoverPanics(securityHeaders(mux)))
}

// protect は、状態を変えるリクエスト（POST / DELETE）が自分のページから来たものだけを next に渡す。
func (s *Server) protect(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross_origin", "別のサイトからの操作は受け付けません。")
			return
		}
		next(w, r)
	})
}

// securityHeaders は全レスポンスに防御用のヘッダを付ける。
func securityHeaders(next http.Handler) http.Handler {
	// Content-Security-Policy: このページが読み込んでよいものを列挙する。
	// 万一 HTML に悪意あるスクリプトが紛れ込んでも、外部サイトへの通信や読み込みをブラウザが止める。
	//   blob: / data:        撮影した写真のプレビュー、3Dライブラリがメモリ上で作る画像や Worker に必要
	//   'wasm-unsafe-eval'   3Dライブラリが WebAssembly を使うため
	//   style 'unsafe-inline' 3Dライブラリやフレームワークが style 属性を使うため
	const csp = "default-src 'self'; " +
		"script-src 'self' 'wasm-unsafe-eval' blob:; " +
		"worker-src 'self' blob:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' blob: data:; " +
		"media-src 'self' blob:; " +
		"connect-src 'self' blob: data:; " +
		"object-src 'none'; base-uri 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		// ブラウザに中身から種類を推測させない（画像のふりをしたスクリプトを実行させない）。
		h.Set("X-Content-Type-Options", "nosniff")
		// 他サイトへ移動するとき、スキャンIDを含む URL を Referer として渡さない。
		h.Set("Referrer-Policy", "no-referrer")
		// 他サイトの iframe に埋め込ませない（クリックジャッキング対策）。
		h.Set("X-Frame-Options", "DENY")
		// カメラは自分のページだけが使える。マイクと位置情報は使わない。
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder はハンドラが書いたステータスコードを覚えておく ResponseWriter。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap は http.ResponseController が元の ResponseWriter に辿り着けるようにする。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests はリクエストごとに1行のログを出す。
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		// 数秒おきに来るヘルスチェックはログを埋めるだけなので出さない。
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		// パスだけを記録し、クエリ文字列は記録しない（将来トークン等が入っても残らないように）。
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// recoverPanics は、ハンドラ内のパニック（プログラムの不具合）でプロセス全体が落ちるのを防ぐ。
// 実行中の復元ジョブを巻き添えにしないため。
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("ハンドラでパニック", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal", "サーバー内部でエラーが発生しました。")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// NewHTTPServer は推奨のタイムアウトを設定した http.Server を返す。
//
// タイムアウトを付ける理由: 付けないと、わざと遅く送る・読む接続にいつまでも付き合わされ、
// 接続数を使い切られる（Slowloris と呼ばれる攻撃）。
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: h,
		// ヘッダは短時間で届くはず。
		ReadHeaderTimeout: 10 * time.Second,
		// ボディ（写真のアップロード）はモバイル回線だと数分かかりうるので長めにする。
		ReadTimeout: 15 * time.Minute,
		// 大きな成果物のダウンロードも遅い回線では時間がかかる。
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
}

// ready は依存先が使える状態かを確かめる。
func (s *Server) ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		return err
	}
	_, err := s.root.FreeBytes()
	return err
}
