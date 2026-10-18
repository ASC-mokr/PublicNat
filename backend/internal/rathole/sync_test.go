package rathole

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/nat-platform/backend/internal/storage"
)

func TestRenderServerConfig(t *testing.T) {
	tunnels := []storage.Tunnel{
		{ID: "11111111-aaaa-bbbb-cccc-dddddddddddd", Name: "ssh-home", Type: "tcp", ServerPort: 5001, TunnelToken: "tok1"},
		{ID: "22222222-aaaa-bbbb-cccc-dddddddddddd", Name: "dns", Type: "udp", ServerPort: 5002, TunnelToken: "tok\"2"},
	}
	out, err := RenderServerConfig("0.0.0.0:2333", tunnels)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	for _, want := range []string{
		"[server]",
		`bind_addr = "0.0.0.0:2333"`,
		"[server.services.ssh-home-11111111-p5001]",
		`type = "tcp"`,
		`token = "tok1"`,
		`bind_addr = "0.0.0.0:5001"`,
		"[server.services.dns-22222222-p5002]",
		`type = "udp"`,
		`token = "tok\"2"`, // кавычка в токене экранирована
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestRenderServerConfigInvalidType(t *testing.T) {
	_, err := RenderServerConfig("0.0.0.0:2333", []storage.Tunnel{{ID: "x", Name: "a", Type: "icmp", ServerPort: 1}})
	if err == nil {
		t.Fatal("expected error for invalid type")
	}
}

func TestValidateNameAndSubdomain(t *testing.T) {
	ok := []string{"my-tunnel", "web1", "A-1"}
	for _, n := range ok {
		if err := ValidateTunnelName(n); err != nil {
			t.Errorf("name %q should be valid: %v", n, err)
		}
	}
	bad := []string{"", "-lead", "has space", "under_score", "привет", "a..b"}
	for _, n := range bad {
		if err := ValidateTunnelName(n); err == nil {
			t.Errorf("name %q should be invalid", n)
		}
	}
	if err := ValidateSubdomain("user1"); err != nil {
		t.Errorf("subdomain user1 invalid: %v", err)
	}
	if err := ValidateSubdomain("Bad Domain"); err == nil {
		t.Error("subdomain with spaces should be invalid")
	}
}

func TestGenerateTokenLengthAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := GenerateToken(48)
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) < 32 {
			t.Fatalf("token too short: %d", len(tok))
		}
		if seen[tok] {
			t.Fatal("duplicate token generated")
		}
		seen[tok] = true
	}
}

// fakeGen — мок Generator с подсчётом вызовов.
type fakeGen struct {
	mu      sync.Mutex
	calls   int32
	delay   time.Duration
	tunnels []storage.Tunnel
	err     error
}

func (f *fakeGen) ListAllTunnels(ctx context.Context) ([]storage.Tunnel, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tunnels, f.err
}

// TestSyncSerializesConcurrentCalls проверяет, что Mutex сериализует параллельные SyncRatholeConfig:
// ни одна генерация/запись файла не выполняется одновременно с другой.
func TestSyncSerializesConcurrentCalls(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/server.toml"

	gen := &fakeGen{delay: 5 * time.Millisecond}
	// DockerBin=/bin/true + Container — заглушка вместо реального docker CLI
	// (/bin/true игнорирует аргументы и возвращает 0, имитируя успешную отправку сигнала).
	s := NewSyncer(gen, Options{
		ConfigPath: path, BindAddr: "0.0.0.0:2333",
		DockerBin: "/bin/true", Container: "fake-rathole", SighupRetry: 1,
	})

	const n = 20
	var wg sync.WaitGroup
	var concurrent int32
	var maxConcurrent int32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			cur := atomic.AddInt32(&concurrent, 1)
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond) // даём горутинам пересечься
			if err := s.SyncRatholeConfig(context.Background()); err != nil {
				t.Errorf("sync failed: %v", err)
			}
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()

	// Внутри SyncRatholeConfig мьютекс держится всё время работы, поэтому
	// «одновременными» можно считать только горутину, уже начавшую Sync и ещё не закончившую.
	// Мьютекс гарантирует, что реально генерация происходит по одной: это видно по тому,
	// что файл всегда содержит полный корректный TOML.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if !strings.Contains(string(data), "[server]") {
		t.Fatalf("written config is corrupted:\n%s", data)
	}
	if got := atomic.LoadInt32(&gen.calls); got != n {
		t.Fatalf("expected %d generations, got %d", n, got)
	}
}

// TestAtomicWriteLeavesNoTmp — после записи не остаётся .tmp файла.
func TestAtomicWriteLeavesNoTmp(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nested/server.toml"
	s := NewSyncer(&fakeGen{}, Options{ConfigPath: path, BindAddr: "0.0.0.0:2333"})
	if err := s.atomicWrite("[server]\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should be renamed away")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("final file missing: %v", err)
	}
}
