package steps

import (
	"fmt"
	"testing"
)

// dagOptSets — наборы опций, при которых граф шагов All() должен быть
// корректным (ацикличным и без висячих зависимостей). Покрываем дефолтную
// комбинацию, регрессию «native без self-node» и обратную совместимость swarm.
func dagOptSets() map[string]*Opts {
	base := func() *Opts {
		return &Opts{
			InstallDir: "/opt/megapolos",
			DBName:     "megapolos",
			DBUser:     "megapolos",
			SvcUser:    "megapolos",
			PgMajor:    16,
			NodeMajor:  18,
		}
	}
	mk := func(mut func(*Opts)) *Opts {
		o := base()
		mut(o)
		return o
	}

	return map[string]*Opts{
		// Дефолтные флаги: native + self-node + GUI — именно этот набор падал
		// на цикле systemd → netmode → bootstrap → token → systemd.
		"native+self-node+gui": mk(func(o *Opts) {
			o.NetworkMode = "native"
			o.AddSelfNode = true
			o.GUI = true
		}),
		// native БЕЗ self-node: регрессия на висячую зависимость systemd → netmode
		// (netmodeStep не добавляется, а systemd на него ссылался).
		"native+no-self-node": mk(func(o *Opts) {
			o.NetworkMode = "native"
			o.AddSelfNode = false
			o.GUI = true
		}),
		// swarm + self-node: обратная совместимость (netmodeStep не добавляется).
		"swarm+self-node": mk(func(o *Opts) {
			o.NetworkMode = "swarm"
			o.AddSelfNode = true
			o.GUI = true
		}),
		// native + GUI как приложение платформы (app-режим): другой набор GUI-шагов.
		"native+gui-app": mk(func(o *Opts) {
			o.NetworkMode = "native"
			o.AddSelfNode = true
			o.GUI = true
			o.GUIApp = true
		}),
		// native + self-node + gui-tls: добавляет gui:tls (D=token).
		"native+self-node+gui-tls": mk(func(o *Opts) {
			o.NetworkMode = "native"
			o.AddSelfNode = true
			o.GUI = true
			o.GUITLS = true
		}),
		// native + base-domain: добавляет base-domain (D=token).
		"native+base-domain": mk(func(o *Opts) {
			o.NetworkMode = "native"
			o.AddSelfNode = true
			o.GUI = true
			o.BaseDomain = "megapolos.local"
		}),
	}
}

func stepNames(steps []Step) []string {
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name())
	}
	return names
}

// TestAllDAGDepsResolvable — каждый шаг All() ссылается только на существующие
// шаги, а имена шагов уникальны. Ловит «висячие» зависимости (как исчезнувший
// netmode при native без self-node).
func TestAllDAGDepsResolvable(t *testing.T) {
	for name, o := range dagOptSets() {
		t.Run(name, func(t *testing.T) {
			steps := All(o)
			known := map[string]bool{}
			for _, s := range steps {
				if known[s.Name()] {
					t.Fatalf("дубликат шага %q", s.Name())
				}
				known[s.Name()] = true
			}
			for _, s := range steps {
				for _, d := range s.Deps() {
					if !known[d] {
						t.Fatalf("шаг %q ссылается на несуществующую зависимость %q (шаги: %v)",
							s.Name(), d, stepNames(steps))
					}
				}
			}
		})
	}
}

// TestAllDAGIsAcyclic — топологическая сортировка (алгоритм Кана). Если
// отсортировать удаётся не все шаги, в графе есть цикл. Это прямая защита от
// регрессии «systemd → netmode → bootstrap → token → systemd».
func TestAllDAGIsAcyclic(t *testing.T) {
	for name, o := range dagOptSets() {
		t.Run(name, func(t *testing.T) {
			steps := All(o)
			if err := topoSort(steps); err != nil {
				t.Fatalf("%v; шаги: %v", err, stepNames(steps))
			}
		})
	}
}

// topoSort возвращает ошибку, если граф циклический или ссылается на
// несуществующий шаг. Чистая проверка на именах/зависимостях, без исполнения
// Detect/Run (никаких обращений к systemd/docker).
func topoSort(steps []Step) error {
	known := map[string]bool{}
	indeg := map[string]int{}
	for _, s := range steps {
		known[s.Name()] = true
		indeg[s.Name()] = 0
	}
	for _, s := range steps {
		for _, d := range s.Deps() {
			if !known[d] {
				return fmt.Errorf("шаг %q ссылается на несуществующую зависимость %q", s.Name(), d)
			}
			indeg[s.Name()]++
		}
	}

	var queue []string
	for _, s := range steps {
		if indeg[s.Name()] == 0 {
			queue = append(queue, s.Name())
		}
	}

	visited := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		visited++
		for _, s := range steps {
			for _, d := range s.Deps() {
				if d == n {
					indeg[s.Name()]--
					if indeg[s.Name()] == 0 {
						queue = append(queue, s.Name())
					}
				}
			}
		}
	}

	if visited != len(steps) {
		return fmt.Errorf("цикл в зависимостях шагов: отсортировано %d из %d", visited, len(steps))
	}
	return nil
}

// TestSystemdDoesNotDependOnNetmode — точечная защита исходной причины бага:
// система-шаг не должен зависеть от netmode, иначе возвращается цикл
// systemd → netmode → bootstrap → token → systemd.
func TestSystemdDoesNotDependOnNetmode(t *testing.T) {
	o := &Opts{
		InstallDir:  "/opt/megapolos",
		NetworkMode: "native",
		AddSelfNode: true,
		GUI:         true,
	}
	for _, s := range All(o) {
		if s.Name() != "systemd" {
			continue
		}
		for _, d := range s.Deps() {
			if d == "netmode" {
				t.Fatal("systemd снова зависит от netmode — вернётся цикл " +
					"systemd → netmode → bootstrap → token → systemd")
			}
		}
		return
	}
	t.Fatal("в графе нет шага systemd")
}
