// internal/daemon/downloader.go — загрузчик бинарника rathole с GitHub Releases
// на чистом net/http (без curl/wget). Поддержка кроссплатформенности через
// runtime.GOOS / runtime.GOARCH.
package daemon

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// RatholeVersion — версия rathole, совпадает с RATHOLE_VERSION в деплое сервера.
	RatholeVersion = "v0.5.0"
	downloadBase   = "https://github.com/rathole-org/rathole/releases/download"
	maxDownloadSz  = 64 << 20 // разумный предел 64 MiB
)

// BinPath — путь к бинарнику rathole в ~/.pnat/bin/.
func BinPath() (string, error) {
	dir, err := homeDir()
	if err != nil {
		return "", err
	}
	name := "rathole"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(dir, "bin", name), nil
}

// targetTriple возвращает triple Rust-таргета для текущей ОС/архитектуры.
func targetTriple(goos, goarch string) (string, error) {
	switch goos {
	case "linux":
		switch goarch {
		case "amd64":
			return "x86_64-unknown-linux-gnu", nil
		case "arm64":
			return "aarch64-unknown-linux-musl", nil
		case "arm":
			return "armv7-unknown-linux-musleabihf", nil
		}
	case "darwin":
		switch goarch {
		case "amd64":
			return "x86_64-apple-darwin", nil
		case "arm64":
			return "aarch64-apple-darwin", nil
		}
	case "windows":
		if goarch == "amd64" {
			return "x86_64-pc-windows-msvc", nil
		}
	}
	return "", fmt.Errorf("платформа %s/%s не поддерживается релизами rathole %s", goos, goarch, RatholeVersion)
}

// EnsureBinary проверяет наличие исполняемого rathole в ~/.pnat/bin; при отсутствии
// скачивает zip-архив релиза через net/http, извлекает бинарник и ставит chmod 0755.
func EnsureBinary() (string, error) {
	path, err := BinPath()
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0 {
		return path, nil // уже на месте и исполняемый
	}

	triple, err := targetTriple(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/%s/rathole-%s.zip", downloadBase, RatholeVersion, triple)

	fmt.Printf("[..] Бинарник rathole не найден — скачиваю %s ...\n", url)
	data, err := httpDownload(url)
	if err != nil {
		return "", fmt.Errorf("скачивание rathole (%s): %w", url, err)
	}

	bin, err := extractBinaryFromZip(data, "rathole")
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("создание %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".download"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return "", fmt.Errorf("запись %s: %w (нет прав на запись в ~/.pnat?)", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("переименование временного файла: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o755); err != nil {
			return "", fmt.Errorf("chmod +x %s: %w", path, err)
		}
	}
	fmt.Printf("[Успешно] rathole %s установлен в %s\n", RatholeVersion, path)
	return path, nil
}

// httpDownload — загрузка по URL с редиректами (GitHub отдаёт 302 на CDN) и лимитом размера.
func httpDownload(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("404: релиз не найден (проверьте версию, список: https://github.com/rathole-org/rathole/releases)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	lr := io.LimitReader(resp.Body, maxDownloadSz+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownloadSz {
		return nil, fmt.Errorf("архив подозрительно большого размера (> %d MiB)", maxDownloadSz>>20)
	}
	return data, nil
}

// extractBinaryFromZip извлекает из zip-архива файл с указанным именем (в корне или подпапке).
func extractBinaryFromZip(zipData []byte, wantName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, fmt.Errorf("повреждённый zip-архив rathole: %w", err)
	}
	_ = zr.File
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		stripped := strings.TrimSuffix(base, ".exe")
		if stripped == wantName && !f.FileInfo().IsDir() {
			if f.UncompressedSize64 > maxDownloadSz {
				return nil, fmt.Errorf("файл %s в архиве подозрительно большого размера", f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(rc, maxDownloadSz))
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("извлечение %s из архива: %w", f.Name, err)
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("в архиве не найден бинарник %q (файлы в архиве: %d)", wantName, len(zr.File))
}
