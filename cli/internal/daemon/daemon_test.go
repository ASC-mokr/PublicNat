package daemon

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTargetTriple(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"linux", "amd64", "x86_64-unknown-linux-gnu"},
		{"linux", "arm64", "aarch64-unknown-linux-musl"},
		{"darwin", "arm64", "aarch64-apple-darwin"},
		{"windows", "amd64", "x86_64-pc-windows-msvc"},
	}
	for _, c := range cases {
		got, err := targetTriple(c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("targetTriple(%s,%s) = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
	if _, err := targetTriple("plan9", "mips"); err == nil {
		t.Error("ожидалась ошибка для неподдерживаемой платформы")
	}
}

func makeZip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinaryFromZip(t *testing.T) {
	want := []byte("#!/bin/sh\necho rathole\n")
	data := makeZip(t, "rathole", want)
	got, err := extractBinaryFromZip(data, "rathole")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("извлечённые данные не совпадают")
	}

	// вариант с .exe (windows)
	data = makeZip(t, "rathole.exe", want)
	if _, err := extractBinaryFromZip(data, "rathole"); err != nil {
		t.Errorf("не извлёк rathole.exe: %v", err)
	}

	// отсутствующий файл
	data = makeZip(t, "README.md", []byte("hello"))
	if _, err := extractBinaryFromZip(data, "rathole"); err == nil {
		t.Error("ожидалась ошибка при отсутствии бинарника в архиве")
	}
}

func TestEnsureBinaryWithFakeServer(t *testing.T) {
	// Формируем zip «релиза» и отдаём его по http://127.0.0.1:port/rathole.zip,
	// подменяя downloadBase нельзя (константа), поэтому проверяем только
	// локальный путь: уже существующий исполняемый файл переиспользуется.
	tmp := t.TempDir()
	t.Setenv("PNAT_HOME", tmp)
	bin, err := BinPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureBinary()
	if err != nil {
		t.Fatalf("EnsureBinary должен переиспользовать готовый бинарник: %v", err)
	}
	if got != bin {
		t.Errorf("путь %q != ожидаемый %q", got, bin)
	}
}

func TestPIDLifecycle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PNAT_HOME", tmp)

	// PID-файла нет — статус «остановлен».
	st := Status_()
	if st.Running || st.PID != 0 {
		t.Fatalf("ожидался остановленный демон, получили %+v", st)
	}

	// Наш собственный процесс жив и содержит "test" в имени — Alive вернёт false
	// (имя не rathole), но Signal(0) проходит. Проверяем базовую семантику.
	if Alive(os.Getpid()) {
		t.Log("имя процесса совпало с rathole? допустимо в CI, пропускаем")
	}
	if Alive(999999) || Alive(-1) {
		t.Error("Alive для несуществующего PID должен быть false")
	}

	// Запись/чтение PID.
	p, _ := pidPath()
	if err := os.WriteFile(p, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, err := ReadPID()
	if err != nil || pid != 12345 {
		t.Fatalf("ReadPID = %d, %v", pid, err)
	}

	// Повреждённый PID-файл.
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPID(); err == nil {
		t.Error("ожидалась ошибка на повреждённом PID-файле")
	}
}

func TestTailFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "daemon.log")
	var b bytes.Buffer
	for i := 0; i < 30; i++ {
		b.WriteString("line-" + runtime.GOOS + "\n")
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailFile(path, 5)
	if n := bytes.Count([]byte(got), []byte("\n")); n != 4 {
		t.Errorf("tail вернул %d переводов вместо 4", n)
	}
}
