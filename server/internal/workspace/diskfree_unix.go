//go:build unix

package workspace

import (
	"fmt"
	"syscall"
)

// FreeBytes は scans/ があるファイルシステムの空き容量（一般ユーザーが使える分）を返す。
//
// なぜ必要か: PVC は容量が決まっている。空きが無い状態で写真を受け取ると、復元の途中で
// 「No space left on device」になり、原因の分かりにくい失敗になる。受付の時点で断る方が親切。
//
// 仕組み: statfs システムコールは「ブロックサイズ」と「空きブロック数」を返すので掛け算する。
// Bavail は root 以外が使える空き（Bfree は root 用の予約分を含む）。
func (r *Root) FreeBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(r.dir, &st); err != nil {
		return 0, fmt.Errorf("空き容量を取得できません: %w", err)
	}
	// フィールドの型は OS ごとに違う（Linux は int64、macOS は uint32 など）ので明示的に変換する。
	return int64(st.Bavail) * int64(st.Bsize), nil
}
