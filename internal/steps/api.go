package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// apiQuery — POST GraphQL к локальному API ядра.
// Авторизация — заголовок `token` (НЕ Authorization: Bearer).
// Возвращает data; errors из ответа превращаются в error.
func apiQuery(c *Ctx, token, query string, variables map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:5100/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("API ответ не JSON (HTTP %d): %.200s", resp.StatusCode, raw)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("API errors: %s", parsed.Errors[0].Message)
	}
	return parsed.Data, nil
}

// selfNodeStep: добавить этот хост нодой в мегаполос (root@127.0.0.1:22).
// Платформа ходит на ноды по ssh2, порт 22 зашит в коде (ExternalProcess.ts),
// auth только по паролю (NodeInput: host/user/password).
func selfNodeStep() Step {
	return StepFunc{
		N: "self-node", D: []string{"token"},
		DetectF: func(c *Ctx) (bool, string) {
			if c.O.Token == "" {
				return false, ""
			}
			found, err := nodeExists(c, c.O.Token, c.O.Hostname)
			if err == nil && found {
				return true, "нода " + c.O.Hostname + " уже существует"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			if err := sh(c, w, "echo 'root:"+c.O.NodeRootPassword+"' | chpasswd"); err != nil {
				return err
			}
			if err := writeFile(c, w, "/etc/ssh/sshd_config.d/60-megapolos-root.conf", RenderSshdDropin(), 0o644, ""); err != nil {
				return err
			}
			// cloud-init дроп-ины запрещают парольную аутентификацию
			if err := sh(c, w, "rm -f /etc/ssh/sshd_config.d/50-cloud-init.conf /etc/ssh/sshd_config.d/60-cloudimg-settings.conf"); err != nil {
				return err
			}
			if err := sh(c, w, "sshd -t && (systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true)"); err != nil {
				return err
			}
			found, err := nodeExists(c, c.O.Token, c.O.Hostname)
			if err != nil {
				return fmt.Errorf("getAllNode: %w", err)
			}
			if found {
				fmt.Fprintf(w, "нода %s уже существует — пропускаю\n", c.O.Hostname)
				return nil
			}
			data, err := apiQueryWait(c, w, c.O.Token,
				"mutation CreateNode($values: NodeInput!) { createNode(values: $values) { id name autoCreateInstances } }",
				map[string]any{"values": map[string]any{
					"name":                c.O.Hostname,
					"host":                "127.0.0.1",
					"user":                "root",
					"password":            c.O.NodeRootPassword,
					"autoCreateInstances": true, // нода по умолчанию для инстансов
				}})
			if err != nil {
				return fmt.Errorf("createNode: %w", err)
			}
			fmt.Fprintf(w, "нода %s добавлена: %s\n", c.O.Hostname, data)
			return nil
		},
	}
}

// apiQueryWait — apiQuery с ожиданием поднятия API: после старта сервиса
// JWT печатается в лог раньше, чем GraphQL начинает слушать :5100.
// Ретраим только сетевые ошибки (connect refused и т.п.), до ~2 минут.
func apiQueryWait(c *Ctx, w io.Writer, token, query string, variables map[string]any) (json.RawMessage, error) {
	var last error
	for i := 0; i < 40; i++ {
		data, err := apiQuery(c, token, query, variables)
		if err == nil {
			return data, nil
		}
		var netErr net.Error
		if !errors.As(err, &netErr) && !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err // логическая ошибка API — не ретраим
		}
		last = err
		if i == 0 {
			fmt.Fprintln(w, "API ещё не слушает :5100 — жду…")
		}
		select {
		case <-c.Done():
			return nil, c.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return nil, last
}

// baseDomainStep: базовый домен инстансов (<instance>-<role>.<baseDomain>).
// В devMode сертификаты под него подписывает Megapolos Root CA (self-signed);
// в prod — certbot/Let's Encrypt (нужен публичный домен + DNS на ноду).
func baseDomainStep() Step {
	return StepFunc{
		N: "base-domain", D: []string{"token"},
		DetectF: func(c *Ctx) (bool, string) {
			if c.O.Token == "" {
				return false, ""
			}
			found, err := domainExists(c, c.O.Token, c.O.BaseDomain)
			if err == nil && found {
				return true, "домен " + c.O.BaseDomain + " уже есть"
			}
			return false, ""
		},
		RunF: func(c *Ctx, w io.Writer) error {
			found, err := domainExists(c, c.O.Token, c.O.BaseDomain)
			if err != nil {
				return fmt.Errorf("getAllDomain: %w", err)
			}
			if found {
				fmt.Fprintf(w, "домен %s уже существует — пропускаю\n", c.O.BaseDomain)
				return nil
			}
			data, err := apiQueryWait(c, w, c.O.Token,
				"mutation CreateDomain($values: DomainInput!) { createDomain(values: $values) { id name isBaseDomain } }",
				map[string]any{"values": map[string]any{
					"name":         c.O.BaseDomain,
					"isBaseDomain": true,
				}})
			if err != nil {
				return fmt.Errorf("createDomain: %w", err)
			}
			fmt.Fprintf(w, "базовый домен %s добавлен: %s\n", c.O.BaseDomain, data)
			fmt.Fprintf(w, "DNS: настрой wildcard *.%s → IP этой машины (или /etc/hosts).\n", c.O.BaseDomain)
			fmt.Fprintf(w, "devMode: сертификаты подпишет Megapolos Root CA — скачай CA из GUI и добавь в доверенные.\n")
			return nil
		},
	}
}

// domainExists — есть ли домен с таким именем.
func domainExists(c *Ctx, token, name string) (bool, error) {
	data, err := apiQueryWait(c, io.Discard, token, "{ getAllDomain { id name } }", nil)
	if err != nil {
		return false, err
	}
	var parsed struct {
		GetAllDomain []struct {
			Name string `json:"name"`
		} `json:"getAllDomain"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false, err
	}
	for _, d := range parsed.GetAllDomain {
		if d.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// nodeExists — есть ли нода с таким именем.
func nodeExists(c *Ctx, token, name string) (bool, error) {
	data, err := apiQueryWait(c, io.Discard, token, "{ getAllNode { id name } }", nil)
	if err != nil {
		return false, err
	}
	var parsed struct {
		GetAllNode []struct {
			Name string `json:"name"`
		} `json:"getAllNode"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false, err
	}
	for _, n := range parsed.GetAllNode {
		if n.Name == name {
			return true, nil
		}
	}
	return false, nil
}
