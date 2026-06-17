package orch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestAdhocSessionClientSuccess(t *testing.T) {
	var gotAuth, gotCT, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/adhoc" {
			t.Fatalf("path = %q, want /api/adhoc", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tmux":"adhoc-42"}`))
	}))
	defer ts.Close()

	tmuxSession, err := requestAdhocSessionClient(ts.URL, "secret-token", "local", "windows desktop debug")
	if err != nil {
		t.Fatalf("requestAdhocSessionClient error = %v", err)
	}
	if tmuxSession != "adhoc-42" {
		t.Fatalf("tmuxSession = %q, want adhoc-42", tmuxSession)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want Bearer secret-token", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody != `{"title":"windows desktop debug","vm":"local"}` {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestRequestAdhocSessionClientMalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tmux":`))
	}))
	defer ts.Close()

	_, err := requestAdhocSessionClient(ts.URL, "secret-token", "local", "windows desktop debug")
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode /api/adhoc response") {
		t.Fatalf("error = %q, want decode /api/adhoc response", err)
	}
}
