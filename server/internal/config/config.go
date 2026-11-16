// Package config は環境変数から設定を読み、起動前に検証する。
//
// なぜ環境変数なのか: Kubernetes では ConfigMap（公開してよい値）と Secret（秘密の値）を
// どちらも環境変数として Pod に渡せる。設定ファイルを別に持つより配備が単純になる。
//
// なぜ起動時にまとめて検証するのか: 設定ミスは「最初のジョブが数分走ってから失敗」より
// 「起動した瞬間に理由つきで落ちる」方がはるかに直しやすい。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config はサーバー全体の設定。
type Config struct {
	ListenAddr string
	// DatabaseURL は PostgreSQL の接続文字列（postgres://user:pass@host:5432/db）。
	DatabaseURL string
	// DataDir は PVC のマウント先。この下に scans/<id>/ を作る。
	DataDir string
	// WebDir はビルド済み Web（index.html 等）の場所。空なら API だけを提供する。
	WebDir string

	Limits   Limits
	Jobs     Jobs
	Pipeline Pipeline
}

// Limits はアップロードの上限。「無制限に受け付けない」ための値を1か所に集める。
type Limits struct {
	MinImages int
	MaxImages int
	// MaxImageBytes は写真1枚の最大バイト数。
	MaxImageBytes int64
	// MaxUploadBytes は1リクエスト全体の最大バイト数。
	// Cloudflare（Free/Pro）はリクエストボディを 100MB で打ち切るので、それより小さくする。
	MaxUploadBytes int64
	// MaxImagePixels は写真1枚の最大画素数（幅×高さ）。
	// 圧縮率の高い画像で「ファイルは小さいが展開すると巨大」という攻撃（画像爆弾）を防ぐ。
	MaxImagePixels int64
	// MinImageSide は短辺の最小画素数。小さすぎる写真は特徴点が取れない。
	MinImageSide int
	// MinFreeDiskBytes は PVC の空きがこれを下回ったら新規受付を断る閾値。
	MinFreeDiskBytes int64
	// MaxQueuedScans は順番待ちジョブの上限。ジョブは1件ずつしか走らないので、
	// 際限なく受け付けるとディスクと待ち時間が膨らむ。
	MaxQueuedScans int
}

// Jobs はジョブ管理の設定。
type Jobs struct {
	// Retention は受付から削除までの保存期間。
	Retention time.Duration
	// ExpiredRowRetention は「期限切れ」と表示するための行を残しておく期間。
	ExpiredRowRetention time.Duration
	// MaxAttempts は1ジョブの最大着手回数（再起動で中断→やり直しの上限）。
	MaxAttempts int
	// PollInterval はワーカーが待ち行列を見に行く間隔（通知を取りこぼしたときの保険）。
	PollInterval time.Duration
	// JanitorInterval は期限切れ掃除の間隔。
	JanitorInterval time.Duration
}

// MeshMode はメッシュ分岐の作り方。
type MeshMode string

const (
	// MeshModeAuto: COLMAP が CUDA 対応なら dense、そうでなければ sparse。
	MeshModeAuto MeshMode = "auto"
	// MeshModeDense: patch_match_stereo → stereo_fusion → poisson_mesher（CUDA 必須）。
	MeshModeDense MeshMode = "dense"
	// MeshModeSparse: 疎な点群から delaunay_mesher で簡易メッシュ（CPU で動く。粗い）。
	MeshModeSparse MeshMode = "sparse"
)

// Pipeline は外部CLIと復元パラメータの設定。
type Pipeline struct {
	ColmapBin         string
	OpenSplatBin      string
	GltfpackBin       string
	SplatTransformBin string

	// RequireTools が true なら、起動時に colmap を実行できなければ起動を中止する。
	// 画面だけを開発したいとき（COLMAP を入れていない PC）は false にする。
	RequireTools bool

	EnableMesh  bool
	EnableSplat bool
	MeshMode    MeshMode

	// ColmapUseGPU は SIFT の抽出・マッチングに GPU を使うか。
	// サーバー（ディスプレイ無し）では OpenGL 版が動かないので、CUDA が無ければ false にする。
	ColmapUseGPU bool
	// ColmapMaxImageSize は特徴抽出・undistort 時の長辺の上限（画素）。大きいほど遅く精密。
	ColmapMaxImageSize int
	// NumThreads は外部CLIに使わせるスレッド数。0 は「CLI の既定（全コア）」。
	// Kubernetes で CPU limit を掛けるときは limit と同じ数にする（多すぎると性能が落ちる）。
	NumThreads int

	// SplatIterations は OpenSplat の学習反復回数（-n）。CPU は GPU の約100倍遅いので控えめにする。
	SplatIterations int
	// SplatDownscale は学習時に画像を縮小する倍率（-d）。2 なら縦横半分。
	SplatDownscale float64
	// SplatForceCPU が true なら --cpu を付けて CPU 実行を強制する。
	SplatForceCPU bool

	SfMTimeout   time.Duration
	MVSTimeout   time.Duration
	MeshTimeout  time.Duration
	SplatTimeout time.Duration
	SOGTimeout   time.Duration
}

// Load は環境変数を読み、既定値で補い、検証して Config を返す。
//
// 入力: getenv（通常は os.Getenv。テストでは map を渡せるよう関数にしてある）。
// 出力: 検証済みの Config。
// 失敗条件: 必須項目の欠落、数値として読めない値、上限同士の矛盾。
// 見つかった問題は1つずつではなく全部まとめて返す（直しては起動、を繰り返さずに済む）。
func Load(getenv func(string) string) (Config, error) {
	r := &reader{getenv: getenv}

	cfg := Config{
		ListenAddr: r.str("LISTEN_ADDR", ":8080"),
		DataDir:    r.str("DATA_DIR", "/data"),
		WebDir:     r.str("WEB_DIR", ""),
		Limits: Limits{
			MinImages:        r.integer("MIN_IMAGES", 8),
			MaxImages:        r.integer("MAX_IMAGES", 80),
			MaxImageBytes:    r.megabytes("MAX_IMAGE_MB", 15),
			MaxUploadBytes:   r.megabytes("MAX_UPLOAD_MB", 95),
			MaxImagePixels:   int64(r.integer("MAX_IMAGE_MEGAPIXELS", 40)) * 1_000_000,
			MinImageSide:     r.integer("MIN_IMAGE_SIDE", 320),
			MinFreeDiskBytes: r.megabytes("MIN_FREE_DISK_MB", 2048),
			MaxQueuedScans:   r.integer("MAX_QUEUED_SCANS", 5),
		},
		Jobs: Jobs{
			Retention:           r.duration("RETENTION", 24*time.Hour),
			ExpiredRowRetention: r.duration("EXPIRED_ROW_RETENTION", 7*24*time.Hour),
			MaxAttempts:         r.integer("MAX_ATTEMPTS", 2),
			PollInterval:        r.duration("POLL_INTERVAL", 5*time.Second),
			JanitorInterval:     r.duration("JANITOR_INTERVAL", 10*time.Minute),
		},
		Pipeline: readPipeline(r),
	}

	cfg.DatabaseURL = databaseURL(r)
	r.errs = append(r.errs, cfg.validate()...)

	if len(r.errs) > 0 {
		return Config{}, fmt.Errorf("設定に問題があります:\n  - %s", strings.Join(r.errs, "\n  - "))
	}
	return cfg, nil
}

// readPipeline は復元パイプラインの設定を読む。
func readPipeline(r *reader) Pipeline {
	return Pipeline{
		RequireTools:       r.boolean("REQUIRE_TOOLS", true),
		ColmapBin:          r.str("COLMAP_BIN", "colmap"),
		OpenSplatBin:       r.str("OPENSPLAT_BIN", "opensplat"),
		GltfpackBin:        r.str("GLTFPACK_BIN", "gltfpack"),
		SplatTransformBin:  r.str("SPLAT_TRANSFORM_BIN", "splat-transform"),
		EnableMesh:         r.boolean("ENABLE_MESH", true),
		EnableSplat:        r.boolean("ENABLE_SPLAT", true),
		MeshMode:           MeshMode(r.str("MESH_MODE", string(MeshModeAuto))),
		ColmapUseGPU:       r.boolean("COLMAP_USE_GPU", false),
		ColmapMaxImageSize: r.integer("COLMAP_MAX_IMAGE_SIZE", 1600),
		NumThreads:         r.integer("NUM_THREADS", 0),
		SplatIterations:    r.integer("SPLAT_ITERATIONS", 1000),
		SplatDownscale:     r.float("SPLAT_DOWNSCALE", 2),
		SplatForceCPU:      r.boolean("SPLAT_FORCE_CPU", true),
		SfMTimeout:         r.duration("SFM_TIMEOUT", 1*time.Hour),
		MVSTimeout:         r.duration("MVS_TIMEOUT", 2*time.Hour),
		MeshTimeout:        r.duration("MESH_TIMEOUT", 1*time.Hour),
		SplatTimeout:       r.duration("SPLAT_TIMEOUT", 6*time.Hour),
		SOGTimeout:         r.duration("SOG_TIMEOUT", 30*time.Minute),
	}
}

// LoadPipeline は復元パイプラインの設定だけを読んで検証する。
// DB や認証を使わない単体実行ツール（cmd/reconstruct）のための入口。
func LoadPipeline(getenv func(string) string) (Pipeline, error) {
	r := &reader{getenv: getenv}
	p := readPipeline(r)
	r.errs = append(r.errs, p.validate()...)
	if len(r.errs) > 0 {
		return Pipeline{}, fmt.Errorf("設定に問題があります:\n  - %s", strings.Join(r.errs, "\n  - "))
	}
	return p, nil
}

// databaseURL は接続文字列を決める。
//
// 2通りの渡し方を受け付ける:
//  1. DATABASE_URL をそのまま渡す（ローカル開発や既存DBの利用に便利）。
//  2. DB_HOST / DB_PORT / DB_USER / DB_PASSWORD / DB_NAME を別々に渡す。
//
// 2 を用意する理由: Kubernetes ではパスワードだけを Secret から渡したい。URL を文字列連結で
// 組み立てると、パスワードに「@」「/」「:」が入ったとき壊れる。net/url に任せれば必ず
// 正しくエスケープされる。
func databaseURL(r *reader) string {
	if v := r.str("DATABASE_URL", ""); v != "" {
		return v
	}
	host := r.str("DB_HOST", "")
	if host == "" {
		return ""
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(r.str("DB_USER", "scanner"), r.str("DB_PASSWORD", "")),
		Host:   host + ":" + r.str("DB_PORT", "5432"),
		Path:   "/" + r.str("DB_NAME", "scanner"),
	}
	q := url.Values{}
	q.Set("sslmode", r.str("DB_SSLMODE", "disable"))
	u.RawQuery = q.Encode()
	return u.String()
}

// validate は値同士の整合性を確かめ、問題の説明（日本語）を返す。
func (c Config) validate() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.DatabaseURL == "" {
		add("DATABASE_URL（または DB_HOST）が未設定です")
	}
	l := c.Limits
	if l.MinImages < 3 {
		add("MIN_IMAGES は3以上にしてください（SfM は最低でも3枚程度の重なった写真が必要です）")
	}
	if l.MaxImages < l.MinImages {
		add("MAX_IMAGES(%d) が MIN_IMAGES(%d) より小さいです", l.MaxImages, l.MinImages)
	}
	if l.MaxImageBytes <= 0 || l.MaxUploadBytes <= 0 || l.MaxImagePixels <= 0 {
		add("MAX_IMAGE_MB / MAX_UPLOAD_MB / MAX_IMAGE_MEGAPIXELS は正の値にしてください")
	}
	if l.MaxUploadBytes < l.MaxImageBytes {
		add("MAX_UPLOAD_MB が MAX_IMAGE_MB より小さいです")
	}
	if l.MaxQueuedScans < 1 {
		add("MAX_QUEUED_SCANS は1以上にしてください")
	}
	if c.Jobs.Retention < time.Minute {
		add("RETENTION が短すぎます（処理が終わる前に削除されます）")
	}
	if c.Jobs.MaxAttempts < 1 {
		add("MAX_ATTEMPTS は1以上にしてください")
	}
	if c.Jobs.PollInterval <= 0 || c.Jobs.JanitorInterval <= 0 {
		add("POLL_INTERVAL / JANITOR_INTERVAL は正の値にしてください")
	}
	return append(errs, c.Pipeline.validate()...)
}

// validate は復元パイプラインの設定を確かめる。
func (p Pipeline) validate() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	switch p.MeshMode {
	case MeshModeAuto, MeshModeDense, MeshModeSparse:
	default:
		add("MESH_MODE は auto / dense / sparse のいずれかにしてください（現在: %q）", p.MeshMode)
	}
	if p.SplatIterations < 1 {
		add("SPLAT_ITERATIONS は1以上にしてください")
	}
	if p.SplatDownscale < 1 {
		add("SPLAT_DOWNSCALE は1以上にしてください（1 は縮小なし）")
	}
	if p.ColmapMaxImageSize < 200 {
		add("COLMAP_MAX_IMAGE_SIZE が小さすぎます")
	}
	if p.NumThreads < 0 {
		add("NUM_THREADS は0以上にしてください（0 は自動）")
	}
	for name, d := range map[string]time.Duration{
		"SFM_TIMEOUT": p.SfMTimeout, "MVS_TIMEOUT": p.MVSTimeout, "MESH_TIMEOUT": p.MeshTimeout,
		"SPLAT_TIMEOUT": p.SplatTimeout, "SOG_TIMEOUT": p.SOGTimeout,
	} {
		if d <= 0 {
			add("%s は正の値にしてください", name)
		}
	}
	return errs
}

// reader は環境変数を型つきで読む小さな補助。読めなかった値のエラーを溜めていく。
type reader struct {
	getenv func(string) string
	errs   []string
}

func (r *reader) str(key, def string) string {
	if v := strings.TrimSpace(r.getenv(key)); v != "" {
		return v
	}
	return def
}

func (r *reader) integer(key string, def int) int {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s は整数で指定してください（現在: %q）", key, v))
		return def
	}
	return n
}

func (r *reader) float(key string, def float64) float64 {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s は数値で指定してください（現在: %q）", key, v))
		return def
	}
	return f
}

func (r *reader) boolean(key string, def bool) bool {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s は true / false で指定してください（現在: %q）", key, v))
		return def
	}
	return b
}

// megabytes は「MB 単位の整数」をバイト数にして返す。
func (r *reader) megabytes(key string, defMB int) int64 {
	return int64(r.integer(key, defMB)) * 1024 * 1024
}

// duration は "24h" "30m" のような Go の時間表記を読む。
func (r *reader) duration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s は 24h や 30m のように指定してください（現在: %q）", key, v))
		return def
	}
	return d
}

// FromOS は実際の環境変数から読む。main から呼ぶ入口。
func FromOS() (Config, error) {
	return Load(os.Getenv)
}
