package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestValidateName(t *testing.T) {
	ok := []string{"web", "my-tunnel", "db_1", "a1"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("имя %q должно быть валидным: %v", n, err)
		}
	}
	bad := []string{"", "-lead", "Привет", "with space", strings.Repeat("a", 65)}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("имя %q должно быть невалидным", n)
		}
	}
}

func TestRenderClientToml(t *testing.T) {
	tunnels := map[string]Tunnel{
		"web": {Name: "web", Type: TypeTCP, ServerPort: 5001, Token: "tok-web", LocalAddr: "127.0.0.1:8080"},
		"dns": {Name: "dns", Type: TypeUDP, ServerPort: 5002, Token: "tok-dns", LocalAddr: "127.0.0.1:53"},
	}
	text, err := RenderClientToml("176.109.108.97:2333", tunnels)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Client struct {
			RemoteAddr string `toml:"remote_addr"`
			Services   map[string]struct {
				Type      string `toml:"type"`
				LocalAddr string `toml:"local_addr"`
				Token     string `toml:"token"`
			} `toml:"services"`
		} `toml:"client"`
	}
	if _, err := toml.Decode(text, &parsed); err != nil {
		t.Fatalf("сгенерированный TOML не парсится: %v\n%s", err, text)
	}
	if parsed.Client.RemoteAddr != "176.109.108.97:2333" {
		t.Errorf("remote_addr = %q", parsed.Client.RemoteAddr)
	}
	if len(parsed.Client.Services) != 2 {
		t.Fatalf("секций services = %d, want 2", len(parsed.Client.Services))
	}
	svc := parsed.Client.Services["web"]
	if svc.Type != "tcp" || svc.LocalAddr != "127.0.0.1:8080" || svc.Token != "tok-web" {
		t.Errorf("секция web: %+v", svc)
	}
	if dns := parsed.Client.Services["dns"]; dns.Type != "udp" || dns.LocalAddr != "127.0.0.1:53" {
		t.Errorf("секция dns: %+v", dns)
	}
}

func TestRenderRejectsInvalid(t *testing.T) {
	if _, err := RenderClientToml("h:1", map[string]Tunnel{"x": {Name: "x", Type: "sctp", Token: "t", LocalAddr: "127.0.0.1:1"}}); err == nil {
		t.Error("ожидалась ошибка на неизвестном типе")
	}
	if _, err := RenderClientToml("h:1", map[string]Tunnel{"x": {Name: "x", Type: TypeTCP, Token: "", LocalAddr: "127.0.0.1:1"}}); err == nil {
		t.Error("ожидалась ошибку на пустом токене")
	}
}

func TestStateSaveLoadAndToml(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PNAT_HOME", tmp)

	st, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ServerURL != DefaultServerURL {
		t.Errorf("дефолтный server_url = %q", st.ServerURL)
	}
	st.JWT = "jwt-token"
	if err := st.UpsertTunnel(Tunnel{Name: "web", Type: TypeTCP, ServerPort: 5001, Token: "tok", LocalAddr: "127.0.0.1:8080"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	// права на config.json должны быть 0600 (там JWT)
	fi, err := os.Stat(filepath.Join(tmp, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("права config.json = %v, want 0600", fi.Mode().Perm())
	}

	st2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if st2.JWT != "jwt-token" || st2.Tunnels["web"].ServerPort != 5001 {
		t.Fatalf("round-trip потерян: %+v", st2)
	}

	path, err := st2.WriteClientToml()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `[client.services.web]`) || !strings.Contains(string(b), `token = "tok"`) {
		t.Errorf("client.toml неполон:\n%s", b)
	}

	// удаление туннеля убирает секцию
	if err := st2.RemoveTunnel("web"); err != nil {
		t.Fatal(err)
	}
	path, err = st2.WriteClientToml()
	if err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "[client.services.web]") {
		t.Error("секция удалённого туннеля осталась в client.toml")
	}
}

func TestJSONShape(t *testing.T) {
	st := State{ServerURL: "u", RemoteAddr: "r", JWT: "j", Tunnels: map[string]Tunnel{
		"web": {Name: "web", Type: TypeTCP, ServerPort: 5001, Token: "t", LocalAddr: "127.0.0.1:80"},
	}}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"server_url"`, `"remote_addr"`, `"jwt"`, `"tunnels"`, `"server_port"`, `"local_addr"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("в JSON нет ключа %s: %s", key, b)
		}
	}
}
