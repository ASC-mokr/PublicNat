//go:build !windows

package daemon

import (
"os/exec"
"syscall"
)

// detach запускает процесс в новой сессии, чтобы он пережил завершение CLI
// и не получал сигналы от терминала пользователя.
func detach(cmd *exec.Cmd) {
cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// setHideWindow — no-op для POSIX-систем.
func setHideWindow(cmd *exec.Cmd) {}
