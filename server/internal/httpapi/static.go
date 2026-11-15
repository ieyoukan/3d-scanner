package httpapi

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// staticHandler はビルド済みの Web（Vite の出力）を返すハンドラを作る。
//
// この Web は SPA（Single Page Application）で、/scans/<id> のような URL はサーバー上の
// ファイルではなく、ブラウザ内の JavaScript が解釈する「画面の名前」。
// そのため、該当するファイルが無いパスには index.html を返し、あとは JavaScript に任せる。
// こうしないと、/scans/<id> を再読み込みしたりリンクを直接開いたりしたときに 404 になる。
func (s *Server) staticHandler() http.Handler {
	if s.cfg.WebDir == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "no_web", "このサーバーは API のみを提供しています（WEB_DIR が未設定）。")
		})
	}
	root := s.cfg.WebDir
	files := http.FileServer(http.Dir(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "このパスでは使えないメソッドです。")
			return
		}
		// path.Clean で "/a/../b" や "//" を正規化する。先頭に "/" を付けてから Clean すると
		// 結果は必ず "/" で始まり ".." を含まないので、root の外には出られない。
		clean := path.Clean("/" + r.URL.Path)
		full := filepath.Join(root, filepath.FromSlash(clean))

		if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
			// Vite は assets/ 以下のファイル名に内容のハッシュを入れる（例: index-3f2a1b.js）。
			// 中身が変われば名前も変わるので、長期間キャッシュしてよい。
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}

		// 拡張子のあるパス（.js / .png など）でファイルが無いのは本当の 404。
		// これにまで index.html を返すと、ブラウザが HTML を JavaScript として読もうとして
		// 分かりにくいエラーになる。
		if path.Ext(clean) != "" {
			http.NotFound(w, r)
			return
		}

		// index.html はキャッシュさせない。古い index.html が残ると、デプロイ後も
		// 既に存在しない古い JavaScript を読みに行って画面が真っ白になる。
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})
}
