package steps

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderCoreConfig(t *testing.T) {
	s := RenderCoreConfig("SECRET", "megapolos", "PASS123", "megapolos", false, true)
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("невалидный JSON: %v\n%s", err, s)
	}
	if m["secret"] != "SECRET" {
		t.Errorf("secret: %v", m["secret"])
	}
	if m["connectionString"] != "postgres://megapolos:PASS123@localhost:5432/megapolos" {
		t.Errorf("connectionString: %v", m["connectionString"])
	}
	if m["devMode"] != true || m["debug"] != false || m["noRoot"] != true {
		t.Errorf("flags: %v", m)
	}
}

func TestRenderGUIConfig(t *testing.T) {
	s := RenderGUIConfig("http://localhost:5100")
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("невалидный JSON: %v", err)
	}
	if m["server"] != "http://localhost:5100" {
		t.Errorf("server: %v", m["server"])
	}
}

func TestRenderCoreUnit_Root(t *testing.T) {
	u := RenderCoreUnit("/opt/megapolos/megapolos-core")
	for _, want := range []string{"User=root", "Group=root", "WorkingDirectory=/opt/megapolos/megapolos-core", "ExecStart=/usr/bin/npm run prod"} {
		if !strings.Contains(u, want) {
			t.Errorf("в юните нет %q:\n%s", want, u)
		}
	}
	if strings.Contains(u, "User=megapolos") {
		t.Error("юнит не должен работать от megapolos (spawn uid:0 → EPERM)")
	}
}

func TestRenderNginxSite(t *testing.T) {
	// GUI static nginx слушает 8080: 80/443 в госте занимает nginx-контейнер ноды
	s := RenderNginxSite("/opt/megapolos/megapolos-gui/build")
	if !strings.Contains(s, "listen 8080 default_server;") || !strings.Contains(s, "root /opt/megapolos/megapolos-gui/build;") {
		t.Errorf("site:\n%s", s)
	}
}

func TestTokenRegex(t *testing.T) {
	log := "  { name: 'root',\n    token: 'eyJhbGciOiJIUzI1NiJ9.abc_def-123.xyz' }\n" +
		"  { name: 'user',\n    token: 'eyJ0ZWFtIjoiZjEyMyJ9.qwe.rty' }\n"
	m := tokenRe.FindAllStringSubmatch(log, -1)
	if len(m) != 2 {
		t.Fatalf("найдено %d токенов, ждали 2", len(m))
	}
	if m[1][1] != "eyJ0ZWFtIjoiZjEyMyJ9.qwe.rty" {
		t.Errorf("последний токен: %q", m[1][1])
	}
}
