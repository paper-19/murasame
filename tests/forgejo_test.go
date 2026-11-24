package tests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"murasame/internal/forgejo"
)

func TestClientStatusesReadsEveryPage(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	total := 60
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/team/app/commits/"+sha+"/statuses" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "token secret" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "[")
		for i := (page - 1) * limit; i < min(page*limit, total); i++ {
			if i > (page-1)*limit {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"id":%d,"context":"job-%d","status":"success","target_url":"http://x/%d",`+
				`"description":"ok"}`, i+1, i, i)
		}
		fmt.Fprint(w, "]")
	}))
	defer srv.Close()
	c := forgejo.New(srv.URL, "team", "app", "secret")
	got, err := c.Statuses(sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Fatalf("%d statuses, want %d", len(got), total)
	}
	if got[0].State != "success" || got[0].Context != "job-0" || got[0].TargetURL != "http://x/0" || got[0].ID != 1 {
		t.Fatalf("first status: %+v", got[0])
	}
}

func TestClientStatusesError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := forgejo.New(srv.URL, "team", "app", "secret").Statuses("abc")
	if !forgejo.HasStatus(err, http.StatusBadGateway) {
		t.Fatalf("got %v, want a 502 error", err)
	}
}
