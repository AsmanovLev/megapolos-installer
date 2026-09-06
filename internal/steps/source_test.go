package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"megapolos/installer/internal/sys"
)

func TestResolveSource(t *testing.T) {
	probe := func(u string) bool { return u == "http://10.0.2.2:8000/" }

	t.Run("bundle явный", func(t *testing.T) {
		s, err := ResolveSource("bundle", "", "/mnt/b", "", probe)
		if err != nil || s.Kind != SrcBundle || s.Base != "/mnt/b" {
			t.Fatalf("got %+v err %v", s, err)
		}
	})
	t.Run("bundle без бандла — ошибка", func(t *testing.T) {
		if _, err := ResolveSource("bundle", "", "", "", probe); err == nil {
			t.Fatal("ожидалась ошибка")
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		s, _ := ResolveSource("gitlab", "", "", "", probe)
		if s.Kind != SrcGitlab || s.Base != GitlabBase {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("local с hostIP", func(t *testing.T) {
		s, _ := ResolveSource("local", "", "", "10.0.2.2", probe)
		if s.Kind != SrcMirror || s.Base != "http://10.0.2.2:8000" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("local без hostIP — ошибка (нет хардкода 10.0.2.2)", func(t *testing.T) {
		if _, err := ResolveSource("local", "", "", "", probe); err == nil {
			t.Fatal("ожидалась ошибка")
		}
	})
	t.Run("auto: бандл побеждает", func(t *testing.T) {
		s, _ := ResolveSource("auto", "", "/mnt/b", "10.0.2.2", probe)
		if s.Kind != SrcBundle {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("auto: живое зеркало", func(t *testing.T) {
		s, _ := ResolveSource("auto", "", "", "10.0.2.2", probe)
		if s.Kind != SrcMirror {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("auto: мёртвое зеркало → gitlab", func(t *testing.T) {
		s, _ := ResolveSource("auto", "", "", "192.168.99.99", probe)
		if s.Kind != SrcGitlab {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("auto: нет ничего → gitlab (без 10.0.2.2-хардкода)", func(t *testing.T) {
		called := false
		s, _ := ResolveSource("auto", "", "", "", func(u string) bool { called = true; return true })
		if s.Kind != SrcGitlab || called {
			t.Fatalf("got %+v probe_called=%v", s, called)
		}
	})
	t.Run("custom URL", func(t *testing.T) {
		s, _ := ResolveSource("custom", "http://my:8000", "", "", probe)
		if s.Kind != SrcMirror || s.Base != "http://my:8000" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("custom путь", func(t *testing.T) {
		s, _ := ResolveSource("custom", "/srv/repos", "", "", probe)
		if s.Kind != SrcPath || s.Base != "/srv/repos" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("URL прямо в choice", func(t *testing.T) {
		s, _ := ResolveSource("https://mirror.corp/megapolos", "", "", "", probe)
		if s.Kind != SrcMirror || s.Base != "https://mirror.corp/megapolos" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("custom пустой — ошибка", func(t *testing.T) {
		if _, err := ResolveSource("custom", "", "", "", probe); err == nil {
			t.Fatal("ожидалась ошибка")
		}
	})
}

func TestSrcURL(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "megapolos-core.git"), 0o755)
	ctx := &Ctx{Context: context.Background(), O: &Opts{}, Ex: sys.Real{}}

	// бандл в приоритете
	ctx.O.BundleDir = "/mnt/b"
	os.MkdirAll("/mnt/b/repos", 0o755) // не создаём сам .git — FileExists по директории repos сработает только на файл; эмулируем наличие
	os.WriteFile("/mnt/b/repos/megapolos-core.git", []byte("x"), 0o644)
	if got := srcURL(ctx, "megapolos-core"); got != "/mnt/b/repos/megapolos-core.git" {
		t.Fatalf("bundle: %s", got)
	}
	ctx.O.BundleDir = ""

	// gitlab
	ctx.O.GitBase, ctx.O.SrcKind = GitlabBase, SrcGitlab
	if got := srcURL(ctx, "megapolos-core"); got != GitlabBase+"/megapolos-core.git" {
		t.Fatalf("gitlab: %s", got)
	}

	// зеркало (dumb-http layout)
	ctx.O.GitBase, ctx.O.SrcKind = "http://10.0.2.2:8000", SrcMirror
	if got := srcURL(ctx, "megapolos-core"); got != "http://10.0.2.2:8000/megapolos-core/.git" {
		t.Fatalf("mirror: %s", got)
	}

	// путь: лейаут <root>/<repo>.git
	ctx.O.GitBase, ctx.O.SrcKind = dir, SrcPath
	if got := srcURL(ctx, "megapolos-core"); got != dir+"/megapolos-core.git" {
		t.Fatalf("path .git: %s", got)
	}

	// путь: лейаут <root>/<repo>/.git
	os.RemoveAll(filepath.Join(dir, "megapolos-core.git"))
	os.MkdirAll(filepath.Join(dir, "megapolos-core", ".git"), 0o755)
	if got := srcURL(ctx, "megapolos-core"); got != dir+"/megapolos-core/.git" {
		t.Fatalf("path dir/.git: %s", got)
	}
}
