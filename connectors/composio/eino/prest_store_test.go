package eino

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockPREST returns an httptest.Server that handles one route:
// /lobehub/public/plugins (the only endpoint RESTAccountStore hits).
// Responses are configured per-test by mutating the returned handler.
func mockPREST(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRESTAccountStore_NotConnected(t *testing.T) {
	srv := mockPREST(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lobehub/public/plugins" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]pluginRow{})
	})

	store := NewRESTAccountStore(srv.URL)
	got, err := store.Resolve(context.Background(), "user-1", "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestRESTAccountStore_HappyPath(t *testing.T) {
	srv := mockPREST(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := pluginRow{Identifier: "gmail", UserID: "user-1"}
		row.CustomParams.Composio = &composioPlugin{
			ConnectedAccountID: "ca_abc123",
			Status:            "ACTIVE",
		}
		_ = json.NewEncoder(w).Encode([]pluginRow{row})
	})

	store := NewRESTAccountStore(srv.URL)
	got, err := store.Resolve(context.Background(), "user-1", "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ca_abc123" {
		t.Errorf("got %q, want ca_abc123", got)
	}
}

func TestRESTAccountStore_PendingStatusReturnsEmpty(t *testing.T) {
	srv := mockPREST(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := pluginRow{Identifier: "gmail", UserID: "user-1"}
		row.CustomParams.Composio = &composioPlugin{
			ConnectedAccountID: "ca_pending",
			Status:            "PENDING",
		}
		_ = json.NewEncoder(w).Encode([]pluginRow{row})
	})

	store := NewRESTAccountStore(srv.URL)
	got, err := store.Resolve(context.Background(), "user-1", "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (PENDING not ACTIVE)", got)
	}
}

func TestRESTAccountStore_WrongUserReturnsEmpty(t *testing.T) {
	srv := mockPREST(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := pluginRow{Identifier: "gmail", UserID: "user-other"}
		row.CustomParams.Composio = &composioPlugin{
			ConnectedAccountID: "ca_someone_else",
			Status:            "ACTIVE",
		}
		_ = json.NewEncoder(w).Encode([]pluginRow{row})
	})

	store := NewRESTAccountStore(srv.URL)
	got, err := store.Resolve(context.Background(), "user-1", "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (cross-user leak prevented)", got)
	}
}

func TestRESTAccountStore_NotFoundReturnsEmpty(t *testing.T) {
	srv := mockPREST(t)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	store := NewRESTAccountStore(srv.URL)
	got, err := store.Resolve(context.Background(), "user-1", "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestNewRESTAccountStore_EmptyReturnsNil(t *testing.T) {
	if s := NewRESTAccountStore(""); s != nil {
		t.Fatal("expected nil")
	}
}

func TestRESTAccountStore_RequiresUserAndApp(t *testing.T) {
	srv := mockPREST(t)
	store := NewRESTAccountStore(srv.URL)
	if _, err := store.Resolve(context.Background(), "", "gmail"); err == nil {
		t.Error("expected error for empty userID")
	}
	if _, err := store.Resolve(context.Background(), "user-1", ""); err == nil {
		t.Error("expected error for empty appIdentifier")
	}
}
