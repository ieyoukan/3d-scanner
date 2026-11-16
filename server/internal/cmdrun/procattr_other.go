//go:build !unix

package cmdrun

import "os/exec"

// Unix 以外（Windows 等）ではプロセスグループを使わず、本体だけを止める。
// 配備先は Linux コンテナなので、ここは「ビルドが通ること」だけが目的。
func configureProcessGroup(*exec.Cmd) {}

func terminate(c *exec.Cmd) error {
	if c.Process == nil {
		return nil
	}
	return c.Process.Kill()
}

// signalName は Unix 以外では常に空（シグナルの概念が違うため）。
func signalName(*exec.ExitError) string { return "" }

func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}
