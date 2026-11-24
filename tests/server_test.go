package tests

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"murasame/internal/server"
	"murasame/internal/service"
	"murasame/internal/states"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func serve(t *testing.T, h http.Handler, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:40000"
	r.Host = "localhost:8737"
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWebhookWakesTheRepositoryItNames(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	q := NewQueue(t, f, baseConfig(f, ""))
	rt := server.NewRuntime([]*service.Service{q.Svc}, time.Hour)
	h := server.Handler(rt, "s3cret")
	body := `{"repository":{"full_name":"team/app","html_url":"http://forge.test/team/app"}}`

	if w := serve(t, h, "POST", "/webhook", body, map[string]string{"X-Forgejo-Signature": "bad"}); w.Code != 403 {
		t.Fatalf("bad signature: %d %s", w.Code, w.Body)
	}
	if q.Svc.CINow() {
		t.Fatal("a rejected webhook woke the service")
	}
	w := serve(t, h, "POST", "/webhook", body, map[string]string{"X-Forgejo-Signature": sign("s3cret", body)})
	if w.Code != 202 || !strings.Contains(w.Body.String(), "queued a sync") {
		t.Fatalf("good signature: %d %s", w.Code, w.Body)
	}
	if !q.Svc.CINow() {
		t.Fatal("the webhook did not ask the CI wait to read the statuses again")
	}
	other := `{"repository":{"full_name":"team/other"}}`
	w = serve(t, h, "POST", "/webhook", other, map[string]string{"X-Forgejo-Signature": sign("s3cret", other)})
	if w.Code != 202 || !strings.Contains(w.Body.String(), "not a repository served here") {
		t.Fatalf("other repository: %d %s", w.Code, w.Body)
	}
}

func TestStatusPageShowsTheQueue(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.OpenPR(1, map[string]string{"BROKEN": "x\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	f.Approve(1)
	q.Tick()
	q.WantState(1, states.SetAside)
	h := server.Handler(server.NewRuntime([]*service.Service{q.Svc}, time.Hour), "")

	w := serve(t, h, "GET", "/api/status", "", nil)
	if w.Code != 200 {
		t.Fatalf("/api/status: %d %s", w.Code, w.Body)
	}
	var page struct {
		Repos []struct {
			Name   string `json:"name"`
			Target string `json:"target"`
			PRs    []struct {
				Number int    `json:"number"`
				State  string `json:"state"`
			} `json:"prs"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if len(page.Repos) != 1 || page.Repos[0].Target != "main" || len(page.Repos[0].PRs) != 1 ||
		page.Repos[0].PRs[0].Number != 1 || page.Repos[0].PRs[0].State != "set_aside" {
		t.Fatalf("status: %s", w.Body)
	}
	if w := serve(t, h, "GET", "/", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "set_aside") {
		t.Fatalf("/: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "gate_slots") {
		t.Fatalf("the status still reports gate slots: %s", w.Body)
	}
}

func TestStatusPageRefusesOtherClients(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	q := NewQueue(t, f, baseConfig(f, ""))
	h := server.Handler(server.NewRuntime([]*service.Service{q.Svc}, time.Hour), "")
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr = "192.0.2.7:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("a remote client got %d", w.Code)
	}
	if w := serve(t, h, "GET", "/healthz", "", nil); w.Code != 200 {
		t.Fatalf("/healthz: %d", w.Code)
	}
}

func TestStatusPageLinksTheRunningCI(t *testing.T) {
	t.Parallel()
	f := NewFakeForge(t)
	f.Pending = 3
	f.OpenPR(1, map[string]string{"a.txt": "a\n"})
	q := NewQueue(t, f, baseConfig(f, ""))
	h := server.Handler(server.NewRuntime([]*service.Service{q.Svc}, time.Hour), "")
	f.Approve(1)
	var during string
	f.OnPoll = func(sha string, n int) {
		if n == 3 {
			during = serve(t, h, "GET", "/api/status", "", nil).Body.String()
		}
	}
	q.Tick()

	if !strings.Contains(during, `"run_url": "http://forge.test/actions/runs/`) {
		t.Fatalf("the status during CI does not link the run: %s", during)
	}
	if strings.Contains(serve(t, h, "GET", "/api/status", "", nil).Body.String(), `"run_url"`) {
		t.Fatal("the run link stayed after the gate")
	}
}
