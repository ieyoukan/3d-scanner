// Package workerapi は、API の Pod からワーカーの Pod へ頼みごとをするための内部用 HTTP。
//
// 役割を分けた構成（ROLE=api と ROLE=worker）では、API は受付・状態・成果物の配信だけを行い、
// 次の 4 つはワーカーに頼む。どれもワーカーのプロセスの中の状態（実行中の外部プロセス）か、
// ワーカーにしか無い道具（COLMAP）が要るため:
//
//	POST   /internal/notify                   待ち行列に入れたので起きて（届かなくても、ワーカーは定期的に見に行く）
//	POST   /internal/scans/{id}/splat         3DGS を作る待ち行列に入れる
//	DELETE /internal/scans/{id}               中止・削除（処理中なら外部プロセスを止める）
//	POST   /internal/scans/{id}/localize      写真1枚の位置合わせ（写真は共有の PVC の一時ディレクトリに置いて、その場所を渡す）
//	GET    /internal/info                     このワーカーで何が出来るか（/api/config の pipeline）
//
// エラーは HTTP の状態コードで返し、クライアントが元のエラー（store.ErrNotFound 等）に戻すので、
// API のハンドラは同じプロセスで動かすとき（ROLE=all）と同じ書き方のままでよい。
//
// この HTTP はクラスタの中だけで使う（Ingress には出さず、NetworkPolicy で API の Pod からだけ届くようにする）。
package workerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ieyoukan/3d-scanner/server/internal/httpapi"
	"github.com/ieyoukan/3d-scanner/server/internal/jobs"
	"github.com/ieyoukan/3d-scanner/server/internal/reconstruction"
	"github.com/ieyoukan/3d-scanner/server/internal/scan"
	"github.com/ieyoukan/3d-scanner/server/internal/store"
	"github.com/ieyoukan/3d-scanner/server/internal/vps"
	"github.com/ieyoukan/3d-scanner/server/internal/workspace"
)

// localizeRequest は位置合わせの依頼。Path は一時ディレクトリ（workspace.Root.TempDir）からの相対パス。
type localizeRequest struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type localizeResult struct {
	Localized      bool                       `json:"localized"`
	Position       [3]float64                 `json:"position"`
	Forward        [3]float64                 `json:"forward"`
	Down           [3]float64                 `json:"down"`
	Camera         *reconstruction.Intrinsics `json:"camera,omitempty"`
	Matches        int                        `json:"matches"`
	ComparedImages int                        `json:"comparedImages"`
	MapImages      int                        `json:"mapImages"`
}

type errorBody struct {
	Code string `json:"code"`
}

// ---------------------------------------------------------------------------
// ワーカー側
// ---------------------------------------------------------------------------

// Jobs はワーカーのジョブ（*jobs.Service）。
type Jobs interface {
	Notify()
	EnqueueSplat(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) (jobs.Removal, error)
}

// Server はワーカーの内部用 HTTP。
type Server struct {
	jobs      Jobs
	localizer httpapi.Localizer
	root      *workspace.Root
	info      httpapi.PipelineInfo
	ready     func(context.Context) error
	log       *slog.Logger
}

// NewServer は内部用 HTTP を作る。ready は /readyz で呼ぶ確認（DB に届くか等）。
func NewServer(js Jobs, loc httpapi.Localizer, root *workspace.Root, info httpapi.PipelineInfo,
	ready func(context.Context) error, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{jobs: js, localizer: loc, root: root, info: info, ready: ready, log: log}
}

// Handler はルートを登録した http.Handler を返す。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.ready != nil {
			if err := s.ready(r.Context()); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /internal/info", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.info) })
	mux.HandleFunc("POST /internal/notify", func(w http.ResponseWriter, _ *http.Request) {
		s.jobs.Notify()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /internal/scans/{id}/splat", s.handleSplat)
	mux.HandleFunc("DELETE /internal/scans/{id}", s.handleRemove)
	mux.HandleFunc("POST /internal/scans/{id}/localize", s.handleLocalize)
	return mux
}

func (s *Server) handleSplat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !scan.ValidID(id) {
		writeCode(w, http.StatusNotFound, "not_found")
		return
	}
	if err := s.jobs.EnqueueSplat(r.Context(), id); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !scan.ValidID(id) {
		writeCode(w, http.StatusNotFound, "not_found")
		return
	}
	res, err := s.jobs.Remove(r.Context(), id)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": string(res)})
}

func (s *Server) handleLocalize(w http.ResponseWriter, r *http.Request) {
	ws, err := s.root.Scan(r.PathValue("id"))
	if err != nil {
		writeCode(w, http.StatusNotFound, "not_found")
		return
	}
	var req localizeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeCode(w, http.StatusBadRequest, "bad_request")
		return
	}
	// 渡されたパスが一時ディレクトリの外を指していたら断る（../ で PVC の他の場所を読ませない）。
	rel := filepath.Clean(req.Path)
	if req.Path == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		writeCode(w, http.StatusBadRequest, "bad_path")
		return
	}
	out, err := s.localizer.Localize(r.Context(), ws, vps.Query{
		Path: filepath.Join(s.root.TempDir(), rel), Width: req.Width, Height: req.Height,
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, localizeResult{
		Localized: out.Localized, Position: out.Position, Forward: out.Forward, Down: out.Down, Camera: out.Camera,
		Matches:        out.Matches,
		ComparedImages: out.ComparedImages, MapImages: out.MapImages,
	})
}

// writeErr はエラーを状態コードにする（クライアントの toError と対になる）。
func (s *Server) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeCode(w, http.StatusNotFound, "not_found")
	case errors.Is(err, store.ErrConflict):
		writeCode(w, http.StatusConflict, "conflict")
	case errors.Is(err, vps.ErrBusy):
		writeCode(w, http.StatusTooManyRequests, "busy")
	case errors.Is(err, vps.ErrTimeout):
		writeCode(w, http.StatusGatewayTimeout, "timeout")
	default:
		s.log.Error("内部 API の処理に失敗", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal")
	}
}

func writeCode(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorBody{Code: code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// API 側（クライアント）
// ---------------------------------------------------------------------------

// Client は API の Pod からワーカーに頼むクライアント。httpapi.Jobs と httpapi.Localizer を満たす。
type Client struct {
	base string
	http *http.Client
	root *workspace.Root
	log  *slog.Logger

	// info は /internal/info の控え（毎回は問い合わせない）。
	mu       sync.Mutex
	info     httpapi.PipelineInfo
	infoAt   time.Time
	fallback httpapi.PipelineInfo
}

// infoTTL は「このワーカーで何が出来るか」を控えておく時間。infoRetry は、届かなかったときに次に試すまでの時間。
const (
	infoTTL   = time.Minute
	infoRetry = 10 * time.Second
)

// NewClient はクライアントを作る。base はワーカーの URL（http://scanner-worker:8081）。
// fallback はワーカーに届かないときに /api/config で返す値（設定から分かる範囲）。
func NewClient(base string, root *workspace.Root, fallback httpapi.PipelineInfo, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		base: strings.TrimRight(base, "/"), root: root, log: log, fallback: fallback,
		// 位置合わせはワーカー側で最大 80 秒かかる（vps.Localizer.Timeout）。それより少し長く待つ。
		http: &http.Client{Timeout: 95 * time.Second},
	}
}

// Notify はワーカーを起こす。待たない（届かなくても、ワーカーは定期的に待ち行列を見に行く）。
func (c *Client) Notify() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.do(ctx, http.MethodPost, "/internal/notify", nil, nil); err != nil {
			c.log.Warn("ワーカーを起こせませんでした（定期的な確認で拾われます）", "error", err)
		}
	}()
}

// EnqueueSplat は 3DGS の作成をワーカーに頼む。
func (c *Client) EnqueueSplat(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/internal/scans/"+id+"/splat", nil, nil)
}

// Remove は中止・削除をワーカーに頼む。
func (c *Client) Remove(ctx context.Context, id string) (jobs.Removal, error) {
	var out struct {
		Result jobs.Removal `json:"result"`
	}
	if err := c.do(ctx, http.MethodDelete, "/internal/scans/"+id, nil, &out); err != nil {
		return "", err
	}
	return out.Result, nil
}

// Localize は位置合わせをワーカーに頼む。写真 q.Path は一時ディレクトリの中に置いておくこと。
func (c *Client) Localize(ctx context.Context, m *workspace.Scan, q vps.Query) (vps.Result, error) {
	rel, err := filepath.Rel(c.root.TempDir(), q.Path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return vps.Result{}, fmt.Errorf("位置合わせの写真が一時ディレクトリの外にあります: %s", q.Path)
	}
	var out localizeResult
	if err := c.do(ctx, http.MethodPost, "/internal/scans/"+m.ID()+"/localize",
		localizeRequest{Path: rel, Width: q.Width, Height: q.Height}, &out); err != nil {
		return vps.Result{}, err
	}
	return vps.Result{
		Localized: out.Localized, Position: out.Position, Forward: out.Forward, Down: out.Down, Camera: out.Camera,
		Matches:        out.Matches,
		ComparedImages: out.ComparedImages, MapImages: out.MapImages,
	}, nil
}

// PipelineInfo は「このワーカーで何が出来るか」を返す（1 分控える。届かなければ設定から分かる値）。
func (c *Client) PipelineInfo(ctx context.Context) httpapi.PipelineInfo {
	c.mu.Lock()
	if !c.infoAt.IsZero() && time.Since(c.infoAt) < infoTTL {
		info := c.info
		c.mu.Unlock()
		return info
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var info httpapi.PipelineInfo
	if err := c.do(ctx, http.MethodGet, "/internal/info", nil, &info); err != nil {
		c.log.Warn("ワーカーの構成を取得できません。設定から分かる値を返します", "error", err)
		// 届かない間に画面を開くたび 3 秒待たせないよう、しばらく（infoRetry）は設定から分かる値を控えておく。
		c.mu.Lock()
		c.info, c.infoAt = c.fallback, time.Now().Add(infoRetry-infoTTL)
		c.mu.Unlock()
		return c.fallback
	}
	c.mu.Lock()
	c.info, c.infoAt = info, time.Now()
	c.mu.Unlock()
	return info
}

// do はワーカーに依頼を送る。in があれば JSON で送り、out があれば応答を読む。
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", httpapi.ErrWorkerUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e errorBody
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		return toError(res.StatusCode, e.Code)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

// toError は状態コードを元のエラーに戻す（Server.writeErr と対になる）。
func toError(status int, code string) error {
	switch status {
	case http.StatusNotFound:
		return store.ErrNotFound
	case http.StatusConflict:
		return store.ErrConflict
	case http.StatusTooManyRequests:
		return vps.ErrBusy
	case http.StatusGatewayTimeout:
		return vps.ErrTimeout
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return fmt.Errorf("%w: 状態 %d", httpapi.ErrWorkerUnavailable, status)
	}
	return fmt.Errorf("ワーカーの内部 API が失敗しました（状態 %d、%s）", status, code)
}
