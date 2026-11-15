//go:build unix

package cmdrun

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup は子プロセスを新しいプロセスグループのリーダーにする。
// グループ ID は子プロセスの PID と同じになる。
func configureProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate はプロセスグループ全体に SIGTERM を送る。
// kill に負の PID を渡すと「そのグループに属する全プロセス」が対象になる。
func terminate(c *exec.Cmd) error {
	if c.Process == nil {
		return nil
	}
	return syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
}
