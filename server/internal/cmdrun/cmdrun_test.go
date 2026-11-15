package cmdrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// このテストは本物の子プロセスを起動する。ただし colmap 等は使わず、
// 「テスト用バイナリ自身」を子プロセスとして起動して、決まった振る舞いをさせる
// （Go 標準ライブラリの os/exec のテストでも使われている方法）。
//
// 環境変数 CMDRUN_HELPER があるとき、このバイナリはテストを走らせる代わりに helper() を実行する。

func TestMain(m *testing.M) {
	if mode := os.Getenv("CMDRUN_HELPER"); mode != "" {
		helper(mode)
		return
	}
	os.Exit(m.Run())
}

// helper は子プロセスとして起動されたときの振る舞い。
func helper(mode string) {
	switch mode {
	case "echo": // 受け取った引数を1行ずつそのまま出力して正常終了
		for _, a := range os.Args[1:] {
			fmt.Println(a)
		}
	case "fail": // 標準エラーに理由を書いて終了コード 3
		fmt.Println("途中までの出力")
		fmt.Fprintln(os.Stderr, "1行目の警告")
		fmt.Fprintln(os.Stderr, "ERROR: 画像が足りません")
		os.Exit(3)
	case "sleep": // 止められるまで待つ
		time.Sleep(60 * time.Second)
	case "ignore-term": // SIGTERM を無視する行儀の悪いプロセス
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(60 * time.Second)
	case "spawn": // 孫プロセスを起動して PID を出力し、待つ
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "CMDRUN_HELPER=sleep")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("child=%d\n", child.Process.Pid)
		time.Sleep(60 * time.Second)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}

// helperCommand はテスト用バイナリ自身を mode で起動する Command を作る。
func helperCommand(t *testing.T, mode string, args ...string) Command {
	t.Helper()
	return Command{
		Name:    os.Args[0],
		Args:    args,
		Env:     []string{"CMDRUN_HELPER=" + mode},
		LogPath: filepath.Join(t.TempDir(), "step.log"),
	}
}

func readLog(t *testing.T, cmd Command) string {
	t.Helper()
	b, err := os.ReadFile(cmd.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunSuccessWritesLog(t *testing.T) {
	cmd := helperCommand(t, "echo", "--database_path", "/data/db")
	res, err := NewExec().Run(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.OutputTail, "--database_path\n/data/db\n") {
		t.Fatalf("出力の末尾が取れていない: %q", res.OutputTail)
	}
	log := readLog(t, cmd)
	for _, want := range []string{"$ " + os.Args[0] + " --database_path /data/db", "/data/db", "[終了: 成功"} {
		if !strings.Contains(log, want) {
			t.Errorf("ログに %q が無い:\n%s", want, log)
		}
	}
}

// TestRunDoesNotUseShell: 引数はシェルに解釈されず、1引数＝1文字列のまま子プロセスに届く。
// コマンドインジェクション対策の確認。もしシェル経由なら INJECTED というファイルが出来てしまう。
func TestRunDoesNotUseShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "INJECTED")
	evil := []string{
		"; touch " + marker,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"a b  c", // 空白を含む引数も分割されない
	}
	cmd := helperCommand(t, "echo", evil...)
	res, err := NewExec().Run(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("引数がシェルとして実行された（%s が存在する）", marker)
	}
	got := strings.Split(strings.TrimRight(res.OutputTail, "\n"), "\n")
	if len(got) != len(evil) {
		t.Fatalf("引数の数が変わった: %q", got)
	}
	for i := range evil {
		if got[i] != evil[i] {
			t.Errorf("引数 %d = %q, want %q", i, got[i], evil[i])
		}
	}
}

func TestRunExitErrorHasCodeAndStderr(t *testing.T) {
	cmd := helperCommand(t, "fail")
	_, err := NewExec().Run(context.Background(), cmd)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if exitErr.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", exitErr.ExitCode)
	}
	if !strings.Contains(exitErr.StderrTail, "1行目の警告") || !strings.Contains(exitErr.StderrTail, "ERROR: 画像が足りません") {
		t.Errorf("stderr の末尾が取れていない: %q", exitErr.StderrTail)
	}
	// エラー文は1行で、最後の行（＝たいてい原因）と終了コードを含む。
	if msg := err.Error(); !strings.Contains(msg, "終了コード 3") || !strings.Contains(msg, "ERROR: 画像が足りません") || strings.Contains(msg, "\n") {
		t.Errorf("エラー文 = %q", msg)
	}
	if log := readLog(t, cmd); !strings.Contains(log, "ERROR: 画像が足りません") || !strings.Contains(log, "[終了: 失敗") {
		t.Errorf("失敗がログに残っていない:\n%s", log)
	}
}

func TestRunTimeout(t *testing.T) {
	cmd := helperCommand(t, "sleep")
	cmd.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := NewExec().Run(context.Background(), cmd)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("時間切れなのに %s も待った", elapsed)
	}
}

func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := helperCommand(t, "sleep")
	cmd.Timeout = time.Minute
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := NewExec().Run(ctx, cmd)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// 取り消しは時間切れとは区別される（画面に出す文言が違うため）。
	if errors.Is(err, ErrTimeout) {
		t.Fatal("取り消しが時間切れとして報告された")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("取り消したのに %s も待った", elapsed)
	}
}

// TestRunKillsProcessThatIgnoresSIGTERM: SIGTERM を無視するプロセスも、猶予の後で必ず止まる。
func TestRunKillsProcessThatIgnoresSIGTERM(t *testing.T) {
	cmd := helperCommand(t, "ignore-term")
	cmd.Timeout = 300 * time.Millisecond
	e := NewExec()
	e.KillDelay = 300 * time.Millisecond
	start := time.Now()
	_, err := e.Run(context.Background(), cmd)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("強制終了されず %s も待った", elapsed)
	}
}

// TestRunCancelStopsGrandchild: 取り消すと、CLI が内部で起動した孫プロセスも一緒に止まる。
func TestRunCancelStopsGrandchild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := helperCommand(t, "spawn")

	done := make(chan error, 1)
	go func() {
		_, err := NewExec().Run(ctx, cmd)
		done <- err
	}()

	// 子プロセスがログに書いた孫の PID を待つ。
	var childPID int
	deadline := time.Now().Add(10 * time.Second)
	for childPID == 0 {
		if time.Now().After(deadline) {
			t.Fatal("孫プロセスの PID が出力されない")
		}
		if b, err := os.ReadFile(cmd.LogPath); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				fmt.Sscanf(line, "child=%d", &childPID)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// kill(pid, 0) は「シグナルを送らずに存在だけ確かめる」。プロセスが居なければ ESRCH。
	deadline = time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return // 孫プロセスは居なくなった
		}
		if time.Now().After(deadline) {
			syscall.Kill(childPID, syscall.SIGKILL) // テストの後始末
			t.Fatalf("取り消し後も孫プロセス %d が生きている", childPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunMissingBinary(t *testing.T) {
	_, err := NewExec().Run(context.Background(), Command{Name: "/nonexistent/colmap-does-not-exist"})
	if err == nil || !strings.Contains(err.Error(), "起動できません") {
		t.Fatalf("err = %v", err)
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		t.Fatal("起動失敗が ExitError として報告された")
	}
}

func TestTailKeepsOnlyTheEnd(t *testing.T) {
	tl := newTail(10)
	tl.Write([]byte("0123456789"))
	tl.Write([]byte("abcde"))
	if got := tl.String(); got != "56789abcde" {
		t.Fatalf("tail = %q", got)
	}
}
