package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// このファイルは「簡易アクセス制限」を実装する。
//
// 何を守るのか:
//   - アップロード: 知らない人に写真を送られて CPU とディスクを使い切られないようにする。
//   - 結果の閲覧: 写真から作った3Dモデルには部屋の中などが写る。URL を知っているだけでは見られないようにする。
//
// 仕組み（共有トークン方式）:
//   - サーバーは環境変数 ACCESS_TOKEN で1つの秘密の文字列を持つ。
//   - ブラウザ: 画面でトークンを入力 → POST /api/session → サーバーが HttpOnly Cookie を返す。
//     以降の通信（API、<a download>、3Dビューアのファイル読み込み）には Cookie が自動で付く。
//   - curl 等: Authorization: Bearer <トークン> ヘッダを付ける。
//
// これは「個人や研究室で使う」ための最小限の仕組みで、利用者ごとのアカウントは無い。
// インターネットに公開するなら Cloudflare Access などを前段に重ねるのがよい（README 参照）。

// sessionCookieBase は Cookie 名の基本部分。
const sessionCookieBase = "scanner_session"

// sessionMaxAge は Cookie の有効期間。
const sessionMaxAge = 30 * 24 * time.Hour

// authenticator はトークンと Cookie の検証を担当する。
type authenticator struct {
	// enabled が false ならすべて許可する（ALLOW_NO_AUTH=true の開発用）。
	enabled bool
	// tokenHash は正しいトークンの SHA-256。比較は常にハッシュどうしで行う（理由は checkToken を参照）。
	tokenHash [32]byte
	// sessionValue は Cookie に入れる値。トークンそのものではなく、トークンから導いた別の値。
	sessionValue string
	secure       bool
}

func newAuthenticator(token string, secure bool) *authenticator {
	a := &authenticator{enabled: token != "", secure: secure}
	if !a.enabled {
		return a
	}
	a.tokenHash = sha256.Sum256([]byte(token))
	// Cookie の値 = HMAC-SHA256(鍵=トークン, メッセージ=固定の文字列)。
	//   - トークンを知っていれば誰でも（＝サーバーだけが）同じ値を計算できる。
	//   - 逆に Cookie の値からトークンは求められない。Cookie が漏れても、
	//     Bearer 認証に使える「トークンそのもの」は漏れない。
	//   - サーバーは何も保存しなくてよい（セッション表が不要）。トークンを変えれば全 Cookie が無効になる。
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("3d-scanner-session-v1"))
	a.sessionValue = hex.EncodeToString(mac.Sum(nil))
	return a
}

// cookieName は Cookie の名前を返す。
// HTTPS のときは "__Host-" を付ける。この接頭辞の付いた Cookie は、ブラウザが
// 「Secure 付き・Path=/・Domain 指定なし」のものしか受け付けないので、
// 同じドメインの別サブドメイン（例: 他のアプリ）から上書きされない。
func (a *authenticator) cookieName() string {
	if a.secure {
		return "__Host-" + sessionCookieBase
	}
	return sessionCookieBase
}

// checkToken は入力されたトークンが正しいかを返す。
//
// 文字列を == で比べない理由: 普通の比較は「最初に違う文字」で打ち切るので、
// 応答時間のわずかな差から先頭何文字が合っているかを推測できてしまう（タイミング攻撃）。
// 両方を SHA-256 で同じ長さにしてから、必ず全バイトを見る比較（ConstantTimeCompare）を使う。
func (a *authenticator) checkToken(token string) bool {
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], a.tokenHash[:]) == 1
}

// authenticated はリクエストが認証済みかを返す。
func (a *authenticator) authenticated(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.checkToken(strings.TrimPrefix(h, "Bearer "))
	}
	if c, err := r.Cookie(a.cookieName()); err == nil {
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.sessionValue)) == 1
	}
	return false
}

// usesCookie は、このリクエストが Cookie で認証されている（Bearer ではない）かを返す。
func (a *authenticator) usesCookie(r *http.Request) bool {
	return a.enabled && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// setCookie はログイン成功時にセッション Cookie を発行する。
func (a *authenticator) setCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:  a.cookieName(),
		Value: a.sessionValue,
		Path:  "/",
		// HttpOnly: JavaScript から読めない。画面に XSS の穴があっても Cookie を盗まれにくい。
		HttpOnly: true,
		// Secure: HTTPS でしか送らない（平文の通信に乗らない）。
		Secure: a.secure,
		// SameSite=Strict: 他のサイトのページから発生したリクエストには付かない。
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionMaxAge.Seconds()),
	})
}

func (a *authenticator) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(), Value: "", Path: "/", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

// sameOrigin は、状態を変えるリクエスト（POST / DELETE）が自分のページから来たものかを確かめる。
//
// なぜ SameSite=Strict だけでは足りないのか:
//
//	SameSite の「サイト」は登録ドメイン単位（例: youkan.uk）。scanner.youkan.uk の Cookie は、
//	other.youkan.uk のページが出したリクエストにも付く。同じドメインで他のアプリも動かしている
//	環境では、そちらのページ経由で勝手にアップロードや削除をされる恐れがある（CSRF）。
//
// 対策: ブラウザは POST / DELETE に必ず Origin ヘッダ（リクエスト元ページのオリジン）を付ける。
// それが自分のホストと一致しなければ拒否する。Origin が無いのはブラウザ以外（curl 等）で、
// そちらは Cookie を勝手に送られる心配が無いので通す。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// attemptLimiter はログイン失敗の回数を数え、総当たりを遅くする。
//
// 方式: 固定ウィンドウ。キー（接続元）ごとに「window の間に max 回まで」。
// 16文字以上のランダムなトークンは総当たりで破れる長さではないが、
// 「何度でも試せる」状態を放置しないための歯止め（多層防御）。
type attemptLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	counts map[string]*attemptWindow
}

type attemptWindow struct {
	start time.Time
	count int
}

func newAttemptLimiter(max int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, now: time.Now, counts: map[string]*attemptWindow{}}
}

// blocked は key がいま制限中かを返す。
func (l *attemptLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.counts[key]
	if !ok || l.now().Sub(w.start) >= l.window {
		return false
	}
	return w.count >= l.max
}

// fail は失敗を1回記録する。
func (l *attemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// 記録が増え続けないよう、期限の切れたものをついでに捨てる。
	if len(l.counts) > 10_000 {
		for k, w := range l.counts {
			if now.Sub(w.start) >= l.window {
				delete(l.counts, k)
			}
		}
	}
	w, ok := l.counts[key]
	if !ok || now.Sub(w.start) >= l.window {
		l.counts[key] = &attemptWindow{start: now, count: 1}
		return
	}
	w.count++
}

// clientKey は制限の単位にする接続元の識別子を返す。
//
// trustProxy が true のときは CF-Connecting-IP（Cloudflare が付ける本当の接続元 IP）を使う。
// Cloudflare Tunnel 経由だと、サーバーから見た接続元は常に cloudflared の Pod になり、
// 全利用者が同じ IP に見えてしまうため。
// このヘッダは Cloudflare を通らない経路では偽装できるので、Tunnel 経由でしか届かない
// 構成のときだけ true にする。
func clientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
