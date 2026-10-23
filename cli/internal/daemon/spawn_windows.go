//go:build windows

package daemon

import (
"os/exec"
"syscall"
)

// detach на Windows обеспечивается флагами создания процесса ниже
// (DETACHED_PROCESS + HideWindow).
func detach(cmd *exec.Cmd) {}

// setHideWindow не показывать консольное окно при запуске демона на Windows.
func setHideWindow(cmd *exec.Cmd) {
if cmd.SysProcAttr == nil {
cmd.SysProcAttr = &syscall.SysProcAttr{}
}
cmd.SysProcAttr.HideWindow = true
cmd.SysProcAttr.CreationFlags |= 0x00000008 // DETACHED_PROCESS
}
