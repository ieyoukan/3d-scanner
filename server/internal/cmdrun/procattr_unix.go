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

// killGroup はプロセスグループ全体を強制終了する（SIGKILL は無視できない）。
func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}

// signalNames は、原因の見当を付けるのに使う主なシグナルの名前。
var signalNames = map[syscall.Signal]string{
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGBUS:  "SIGBUS",
	syscall.SIGFPE:  "SIGFPE",
	syscall.SIGILL:  "SIGILL",
}

// signalName は、プロセスがシグナルで終了していればその名前を返す。そうでなければ空文字。
func signalName(err *exec.ExitError) string {
	status, ok := err.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	if name, ok := signalNames[status.Signal()]; ok {
		return name
	}
	return status.Signal().String()
}
