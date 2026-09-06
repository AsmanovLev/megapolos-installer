package steps

import (
	"fmt"
	"strings"

	"megapolos/installer/internal/sys"
)

// SourceKind — тип источника кода (см. ResolveSource).
type SourceKind int

const (
	SrcBundle SourceKind = iota // оффлайн-бандл (repos внутри)
	SrcGitlab                   // официальная репа https://gitlab.com/megapolos
	SrcMirror                   // http(s)-зеркало (dumb-http, layout <base>/<repo>/.git)
	SrcPath                     // локальный каталог с репо (<root>/<repo>.git или <root>/<repo>/.git)
)

// Source — результат резолва --source.
type Source struct {
	Kind  SourceKind
	Base  string // URL или путь
	Human string // человекочитаемый вердикт для сводки/логов
}

const GitlabBase = "https://gitlab.com/megapolos"

// ResolveSource — единая точка резолва --source (без хардкодов хоста).
//
//	choice:
//	  "bundle"        — только бандл (bundleDir обязателен)
//	  "gitlab"        — официальная репа
//	  "local"         — зеркало хоста http://<hostIP>:8000 (hostIP обязателен)
//	  "custom:<value>"/"custom" + custom — свой URL или путь
//	  "auto"          — bundle → local (если hostIP задан и зеркало живо) → gitlab
//	  "<url|path>"    — как custom:<value>
//
// probe — проверка живости зеркала (в tests подменяется).
func ResolveSource(choice, custom, bundleDir, hostIP string, probe func(string) bool) (Source, error) {
	switch choice {
	case "bundle":
		if bundleDir == "" {
			return Source{}, fmt.Errorf("бандл не найден: укажи --bundle /путь/к/бандлу (каталог с debs/ и installer)")
		}
		return Source{SrcBundle, bundleDir, "бандл " + bundleDir}, nil

	case "gitlab":
		return Source{SrcGitlab, GitlabBase, "официальная репа " + GitlabBase}, nil

	case "local":
		if hostIP == "" {
			return Source{}, fmt.Errorf("зеркало хоста не задано: нет vm.env (HOST_IP) и не указан --host-ip")
		}
		base := "http://" + hostIP + ":8000"
		return Source{SrcMirror, base, "зеркало хоста " + base}, nil

	case "custom":
		return customSource(custom)

	case "auto", "":
		if bundleDir != "" {
			return Source{SrcBundle, bundleDir, "бандл " + bundleDir}, nil
		}
		if hostIP != "" && probe != nil && probe("http://"+hostIP+":8000/") {
			base := "http://" + hostIP + ":8000"
			return Source{SrcMirror, base, "зеркало хоста " + base}, nil
		}
		return Source{SrcGitlab, GitlabBase, "официальная репа " + GitlabBase}, nil

	default:
		// явный URL или путь прямо в --source
		return customSource(choice)
	}
}

func customSource(v string) (Source, error) {
	v = strings.TrimSpace(strings.TrimPrefix(v, "file://"))
	if v == "" {
		return Source{}, fmt.Errorf("пустой кастомный источник")
	}
	switch {
	case strings.HasPrefix(v, "http://"), strings.HasPrefix(v, "https://"):
		return Source{SrcMirror, strings.TrimRight(v, "/"), "зеркало " + v}, nil
	default:
		return Source{SrcPath, strings.TrimRight(v, "/"), "репозитории из " + v}, nil
	}
}

// srcURL — URL/путь для клонирования конкретного репозитория.
func srcURL(c *Ctx, repo string) string {
	// бандл в приоритете: repos лежат рядом с debs
	if c.O.BundleDir != "" {
		p := c.O.BundleDir + "/repos/" + repo + ".git"
		if sys.FileExists(c, c.Ex, p) {
			return p
		}
	}
	switch c.O.SrcKind {
	case SrcMirror:
		return c.O.GitBase + "/" + repo + "/.git" // dumb-http layout зеркала
	case SrcPath:
		// поддерживаем оба лейаута: <root>/<repo>.git и <root>/<repo>/.git
		if sys.FileExists(c, c.Ex, c.O.GitBase+"/"+repo+".git") {
			return c.O.GitBase + "/" + repo + ".git"
		}
		return c.O.GitBase + "/" + repo + "/.git"
	default: // SrcGitlab и любой прочий https
		return c.O.GitBase + "/" + repo + ".git"
	}
}
