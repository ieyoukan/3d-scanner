// Package cmdrun は外部CLI（colmap / opensplat / gltfpack / splat-transform）を安全に実行する。
//
// 守っている決まり:
//  1. シェルを通さない。プログラム名と引数を別々の文字列として OS に渡す（exec.CommandContext）。
//     "sh -c 'colmap ... ' + パス" のような文字列連結をしないので、引数に ; や $() が
//     紛れ込んでも「ただの文字」として扱われ、別のコマンドとして実行されることがない。
//  2. 必ず時間制限を付ける。止まらない外部プロセスがワーカーを永久に塞ぐのを防ぐ。
//  3. キャンセルできる。ctx が取り消されたらプロセス（と、その子プロセス）を止める。
//  4. 出力を全部ログファイルに残す。失敗時は stderr の末尾と終了コードをエラーに含める。
package cmdrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Command は実行する外部コマンド1つ。
type Command struct {
	// Name は実行ファイル（PATH から探す名前、または絶対パス）。
	Name string
	// Args は引数。1要素が1引数になる（空白を含んでいても分割されない）。
	Args []string
	// Dir は作業ディレクトリ。空なら現在のディレクトリ。
	Dir string
	// Env は追加の環境変数（"KEY=value"）。サーバー自身の環境変数に足される。
	Env []string
	// Timeout はこのコマンドの制限時間。0 以下なら制限なし（ctx の期限だけが効く）。
	Timeout time.Duration
	// LogPath は stdout と stderr を追記するファイル。空なら残さない。
	LogPath string
}

// String はログ表示用にコマンドを1行にする（シェルに渡すための文字列ではない）。
func (c Command) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Result は成功したコマンドの結果。
type Result struct {
	Duration time.Duration
	// OutputTail は stdout+stderr の末尾（統計値の読み取りなどに使う）。
	OutputTail string
}

// Runner は外部コマンドを実行する。
//
// インターフェースにしている理由: 復元パイプラインの「どの順で何を呼ぶか」「途中で失敗したら
// どうなるか」を、COLMAP を入れていない環境でもテストできるようにするため。
// テストでは「呼ばれた内容を記録して、決めた結果を返す」偽物を渡す。
type Runner interface {
	// Run はコマンドを最後まで実行する。
	// 失敗条件: 起動できない／終了コードが 0 以外（*ExitError）／時間切れ（ErrTimeout）／
	// ctx の取り消し（context.Canceled を含むエラー）。
	Run(ctx context.Context, cmd Command) (Result, error)
}

// ErrTimeout はコマンドが制限時間内に終わらなかったことを表す。
var ErrTimeout = errors.New("制限時間内に終わりませんでした")

// ExitError はコマンドが 0 以外の終了コードで終わったことを表す。
type ExitError struct {
	Command  string
	ExitCode int
	// StderrTail は標準エラー出力の末尾。原因はたいていここに書かれている。
	StderrTail string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s が終了コード %d で失敗しました", e.Command, e.ExitCode)
	if e.StderrTail != "" {
		msg += ": " + lastLine(e.StderrTail)
	}
	return msg
}

// Exec は実際にプロセスを起動する Runner。
type Exec struct {
	// TailBytes はエラーや結果に含める出力末尾の最大バイト数。
	TailBytes int
	// KillDelay は、停止の合図（SIGTERM）を送ってから強制終了（SIGKILL）するまでの猶予。
	KillDelay time.Duration
}

// NewExec は既定値つきの Exec を返す。
func NewExec() *Exec {
	return &Exec{TailBytes: 8 * 1024, KillDelay: 10 * time.Second}
}

// Run は Runner の実装。
//
// 入力: cmd（プログラム名・引数・制限時間・ログの場所）。
// 出力: 所要時間と出力の末尾。
// 副作用: 子プロセスを起動する。LogPath に「実行したコマンド」「出力」「終了コードと時間」を追記する。
func (e *Exec) Run(ctx context.Context, cmd Command) (Result, error) {
	// 制限時間は ctx に重ねる。呼び出し元の取り消し（キャンセル・シャットダウン）と
	// 時間切れのどちらが起きても、同じ仕組みでプロセスが止まる。
	runCtx := ctx
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(runCtx, cmd.Name, cmd.Args...)
	c.Dir = cmd.Dir
	if len(cmd.Env) > 0 {
		c.Env = append(os.Environ(), cmd.Env...)
	}
	// 子プロセスを独立したプロセスグループに入れ、止めるときはグループごと止める
	// （OS ごとの実装は procattr_*.go）。こうしないと、CLI が内部で起動した孫プロセスが
	// 生き残って CPU を使い続けることがある。
	configureProcessGroup(c)
	// ctx が終わったら、まず SIGTERM で「片付けて終了して」と頼む。
	c.Cancel = func() error { return terminate(c) }
	// 猶予を過ぎても終わらなければ、Go が SIGKILL で強制終了し、出力のパイプも閉じる。
	c.WaitDelay = e.KillDelay

	var logFile *os.File
	if cmd.LogPath != "" {
		f, err := os.OpenFile(cmd.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return Result{}, fmt.Errorf("ログファイルを開けません: %w", err)
		}
		defer f.Close()
		logFile = f
		fmt.Fprintf(f, "\n$ %s\n", cmd.String())
	}

	// 出力は「ログファイル」と「末尾だけ覚えておくバッファ」の両方に流す。
	// 全部をメモリに溜めない: 3DGS の学習は数万行を出すことがある。
	outTail := newTail(e.TailBytes)
	errTail := newTail(e.TailBytes)
	c.Stdout = tee(logFile, outTail)
	c.Stderr = tee(logFile, errTail, outTail)

	start := time.Now()
	runErr := c.Run()
	dur := time.Since(start)

	res := Result{Duration: dur, OutputTail: outTail.String()}
	if logFile != nil {
		fmt.Fprintf(logFile, "[終了: %s / %s]\n", describe(runErr), dur.Round(time.Millisecond))
	}
	if runErr == nil {
		return res, nil
	}

	// 失敗の原因を、呼び出し側が区別できる形に整理する。順番が大事:
	// 時間切れや取り消しでプロセスを止めた場合も c.Run は ExitError を返すので、先に ctx を見る。
	switch {
	case ctx.Err() != nil:
		// 呼び出し元が取り消した（利用者のキャンセル、またはサーバーの停止）。
		return res, fmt.Errorf("%s を中止しました: %w", cmd.Name, ctx.Err())
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return res, fmt.Errorf("%s（制限 %s）: %w", cmd.Name, cmd.Timeout, ErrTimeout)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return res, &ExitError{Command: cmd.Name, ExitCode: exitErr.ExitCode(), StderrTail: errTail.String()}
	}
	// ここに来るのは「起動自体ができなかった」場合（実行ファイルが無い、権限が無いなど）。
	return res, fmt.Errorf("%s を起動できません: %w", cmd.Name, runErr)
}

// describe はログの最後に書く一言を返す。
func describe(err error) string {
	if err == nil {
		return "成功"
	}
	return "失敗 (" + err.Error() + ")"
}

// tee は nil でない書き込み先すべてに同じ内容を書く Writer を返す。
func tee(log *os.File, others ...io.Writer) io.Writer {
	ws := make([]io.Writer, 0, len(others)+1)
	if log != nil {
		ws = append(ws, log)
	}
	ws = append(ws, others...)
	return io.MultiWriter(ws...)
}

// tail は書き込まれた内容の末尾 max バイトだけを覚えておく Writer。
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTail(max int) *tail {
	if max <= 0 {
		max = 4096
	}
	return &tail{max: max}
}

// Write は io.Writer の実装。stdout と stderr の2つの goroutine から呼ばれるのでロックする。
func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "")
}

// lastLine は s の最後の空でない行を返す（エラーメッセージを1行に収めるため）。
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 300 {
				l = l[:300] + "…"
			}
			return l
		}
	}
	return ""
}
