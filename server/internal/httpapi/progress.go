package httpapi

import (
	"io"
	"os"
	"regexp"
	"strconv"
)

// stepProgress は、実行中のステップが「全体のうちどこまで進んだか」の実測値。
//
// 画面に進捗を出すときの決まり: 根拠のある数字だけを出す。
// SfM やメッシュ化は「あと何割か」を外から知る方法が無いので、出さない。
// OpenSplat の学習だけは、ツール自身が1反復ごとに "Step 123: 0.31 [41%]" と出力するので、
// それをログから読み取って「123 / 300 ステップ」と表示できる。
type stepProgress struct {
	Current int    `json:"current"`
	Total   int    `json:"total"`
	Unit    string `json:"unit"`
}

// splatStepPattern は OpenSplat の進行状況の行（"Step 123: 0.3098 [41%]"）に一致する。
var splatStepPattern = regexp.MustCompile(`Step (\d+):`)

// splatProgress は 3DGS 学習ログの末尾から、いま何反復目かを読み取る。
//
// 入力: ログファイルのパスと、設定された総反復数。
// 出力: 進み具合。読み取れなければ nil（画面には何も出さない。推測で埋めない）。
func splatProgress(logPath string, total int) *stepProgress {
	if total <= 0 {
		return nil
	}
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	// ログは数千行になるので、末尾の 4KB だけを読む。最新の行はそこに必ず入っている。
	const tailBytes = 4096
	if fi, err := f.Stat(); err == nil && fi.Size() > tailBytes {
		if _, err := f.Seek(-tailBytes, io.SeekEnd); err != nil {
			return nil
		}
	}
	tail, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return nil
	}
	matches := splatStepPattern.FindAllSubmatch(tail, -1)
	if len(matches) == 0 {
		return nil
	}
	current, err := strconv.Atoi(string(matches[len(matches)-1][1]))
	if err != nil || current < 0 {
		return nil
	}
	// ツールの出力が設定値を超えることは無いはずだが、超えたら表示が破綻しないよう丸める。
	return &stepProgress{Current: min(current, total), Total: total, Unit: "ステップ"}
}
