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
