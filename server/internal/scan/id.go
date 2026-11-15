package scan

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewID は新しいスキャンID（UUID バージョン4）を作る。
//
// なぜサーバー側で乱数から作るのか:
//   - ID はそのままディレクトリ名（/data/scans/<id>）と URL に使う。利用者が決めた文字列を
//     パスに使うと「../」などで外のファイルに届く恐れがある（パストラバーサル）。
//   - 122ビットの乱数なので推測できない。連番だと他人のスキャンのURLを当てられてしまう。
//
// 失敗条件: OS の乱数源が読めないとき（ほぼ起きないが、起きたら ID を作ってはいけない）。
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("乱数を取得できません: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // バージョン 4（乱数由来）
	b[8] = (b[8] & 0x3f) | 0x80 // バリアント RFC 4122
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// ValidID は s が NewID の作る形式（小文字16進の 8-4-4-4-12）かどうかを返す。
//
// API は URL から受け取った ID を、DB やファイルシステムに触る前に必ずこれで確かめる。
// 形式が決まっていれば「/」「.」などパスに意味を持つ文字は絶対に含まれない。
func ValidID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
