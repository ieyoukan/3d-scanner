package httpapi

import (
	"net/http"
	"net/url"
)

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
//
// アプリ自身は認証を持たない（前段の Cloudflare Access などに任せる）が、Access の Cookie は
// 別サイトのページから発生したリクエストにも付き得るので、この確認は残している。
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
