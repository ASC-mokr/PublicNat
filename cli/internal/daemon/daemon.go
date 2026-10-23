// Package daemon управляет фоновым процессом rathole-клиента:
// ровно ОДИН демон на машину обслуживает ВСЕ туннели (мультиплексирование
// RatHole). Клиент rathole НЕ поддерживает hot-reload (SIGHUP), поэтому при
// любом изменении списка туннелей демон убивается по PID и запускается заново
// с перегенерированным ~/.pnat/client.toml.
package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNotRunning — демон не запущен.
var ErrNotRunning = errors.New("демон не запущен")

func pidPath() (string, error) {
	dir, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.pid"), nil
}

func logPath() (string, error) {
	dir, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

func homeDir() (string, error) {
	if h := os.Getenv("PNAT_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("не удалось определить домашнюю директорию: %w", err)
	}
	return filepath.Join(home, ".pnat"), nil
}

// ReadPID читает PID демона из ~/.pnat/daemon.pid. Если файла нет — os.ErrNotExist.
func ReadPID() (int, error) {
	p, err := pidPath()
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("повреждён файл PID: %s", p)
	}
	return pid, nil
}

// Alive проверяет, что процесс с указанным PID жив и является нашим rathole
// (по имени исполняемого файла, чтобы не принять переиспользованный чужой PID).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	exe := processExe(pid)
	if exe == "" {
		return true // не смогли прочитать имя (например, нет прав) — считаем живым
	}
	return strings.Contains(strings.ToLower(filepath.Base(exe)), "rathole")
}

// Status возвращает сведения о текущем демоне.
type Status struct {
	Running bool
	PID     int
	Exe     string
	LogFile string
}

func Status_() Status {
	st := Status{}
	pid, err := ReadPID()
	if err != nil {
		return st
	}
	st.PID = pid
	st.Running = Alive(pid)
	if st.Running {
		st.Exe = processExe(pid)
	}
	if lp, err := logPath(); err == nil {
		st.LogFile = lp
	}
	return st
}

func processExe(pid int) string {
	// Linux: /proc/<pid>/exe. Так как демон запускается через shell-обёртку с
	// exec, в /proc виден реальный бинарник rathole (а не интерпретатор).
	if b, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return b
	}
	// Fallback для систем без /proc (macOS и т.п.): читаем аргументы командной
	// строки через `ps`. Это stdlib-вызов, не внешняя утилита загрузки.
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// killExisting останавливает работающий демон (если есть): SIGTERM, ожидание
// до 5 секунд, затем SIGKILL. Удаляет PID-файл.
func killExisting() error {
	pid, err := ReadPID()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// повреждённый PID-файл — просто убираем его
		if p, perr := pidPath(); perr == nil {
			_ = os.Remove(p)
		}
		return nil
	}
	if !Alive(pid) {
		if p, perr := pidPath(); perr == nil {
			_ = os.Remove(p)
		}
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	fmt.Printf("[..] Останавливаю старый демон rathole (PID %d)...\n", pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("SIGTERM процессу %d: %w", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			break // процесс завершился
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		fmt.Printf("[..] Демон не отреагировал на SIGTERM — SIGKILL %d\n", pid)
		if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("SIGKILL процессу %d: %w", pid, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if p, err := pidPath(); err == nil {
		_ = os.Remove(p)
	}
	return nil
}

// Restart перезапускает демон: убивает старый процесс (по PID), гарантирует
// наличие бинарника rathole (скачивает при необходимости) и запускает новый
// фоновый процесс `rathole --client <clientToml>`, сохраняя PID.
// Возвращает ошибку, если после старта процесс сразу умер (например, неверный
// токен/адрес сервера — подробности в daemon.log).
func Restart(clientToml string) error {
	if _, err := os.Stat(clientToml); err != nil {
		return fmt.Errorf("конфиг демона %s не найден: %w", clientToml, err)
	}
	if err := killExisting(); err != nil {
		return err
	}
	bin, err := EnsureBinary()
	if err != nil {
		return err
	}
	logFile, err := logPath()
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("открытие журнала %s: %w", logFile, err)
	}
	defer lf.Close()

	cmd := exec.Command(bin, "--client", clientToml)
	cmd.Stdout = lf
	cmd.Stderr = lf
	// Отделяем процесс демона от терминала CLI (см. spawn_unix/spawn_windows),
	// чтобы демон пережил завершение pnat.
	detach(cmd)
	setHideWindow(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("запуск демона rathole: %w", err)
	}
	pid := cmd.Process.Pid

	p, err := pidPath()
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if err := os.WriteFile(p, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("запись %s: %w (нет прав на запись в ~/.pnat?)", p, err)
	}

	// Health-check: даём демону секунду на старт и соединение с сервером.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := ""
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.Sys() != nil {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
					if ws.Signaled() {
						code = ": убит сигналом " + ws.Signal().String()
					} else {
						code = ": код выхода " + strconv.Itoa(ws.ExitStatus())
					}
				}
			} else if err != nil {
				code = ": " + err.Error()
			}
		}
		_ = os.Remove(p)
		return fmt.Errorf("демон rathole неожиданно завершился при старте%s. Последние строки журнала %s:\n%s",
			code, logFile, tailFile(logFile, 15))
	case <-time.After(time.Second):
		// процесс жив спустя секунду — считаем старт успешным
	}
	go func() { <-done }() // не копим канал, когда CLI выйдет
	fmt.Printf("[Успешно] Демон rathole перезапущен (PID %d)\n", pid)
	return nil
}

// Stop останавливает демон (используется командой stop при пустом списке туннелей
// или явно пользователем).
func Stop() (bool, error) {
	was := Status_()
	if !was.Running {
		if p, err := pidPath(); err == nil {
			_ = os.Remove(p)
		}
		return false, nil
	}
	if err := killExisting(); err != nil {
		return false, err
	}
	return true, nil
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(журнал недоступен: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
