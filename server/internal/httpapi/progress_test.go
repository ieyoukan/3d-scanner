package httpapi

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSplatProgress(t *testing.T) {
	write := func(content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "splat.log")
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("最後の Step 行を読む", func(t *testing.T) {
		// OpenSplat 1.2.2 の実際の出力形式。
		log := "$ opensplat /work -n 300\nUsing CPU\nStep 1: 0.412 [0%]\nStep 2: 0.398 [0%]\nStep 123: 0.3098 [41%]\n"
		got := splatProgress(write(log), 300)
		if got == nil || got.Current != 123 || got.Total != 300 || got.Unit != "ステップ" {
			t.Fatalf("progress = %+v", got)
		}
	})

	t.Run("長いログでも末尾だけ読んで最新の値を返す", func(t *testing.T) {
		var b strings.Builder
		for i := 1; i <= 1800; i++ {
			b.WriteString("Step " + strconv.Itoa(i) + ": 0.300000 [50%]\n")
		}
		got := splatProgress(write(b.String()), 2000)
		if got == nil || got.Current != 1800 {
			t.Fatalf("progress = %+v", got)
		}
	})

	t.Run("読み取れないときは nil（推測で埋めない）", func(t *testing.T) {
		if got := splatProgress(write("Loading images...\nUsing CPU\n"), 300); got != nil {
			t.Errorf("Step 行が無いのに %+v", got)
		}
		if got := splatProgress(filepath.Join(t.TempDir(), "missing.log"), 300); got != nil {
			t.Errorf("ファイルが無いのに %+v", got)
		}
		if got := splatProgress(write("Step 5: 0.3 [1%]\n"), 0); got != nil {
			t.Errorf("総数が不明なのに %+v", got)
		}
	})

	t.Run("総数を超える値は丸める", func(t *testing.T) {
		if got := splatProgress(write("Step 999: 0.3 [100%]\n"), 300); got == nil || got.Current != 300 {
			t.Fatalf("progress = %+v", got)
		}
	})
}
