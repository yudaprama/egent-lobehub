package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNew_Disabled(t *testing.T) {
	c := New("", "")
	if c.Enabled() {
		t.Fatal("expected disabled client")
	}

	allowed, err := c.Check(context.Background(), CheckParams{
		Namespace: "workspace", Object: "ws1", Relation: "view", SubjectID: "u1",
	})
	if err != nil {
		t.Fatalf("disabled Check should not error: %v", err)
	}
	if !allowed {
		t.Fatal("disabled client should fail-open")
	}

	if err := c.WriteTuple(context.Background(), Tuple{}); err != nil {
		t.Fatalf("disabled WriteTuple should not error: %v", err)
	}
	if err := c.DeleteTuple(context.Background(), Tuple{}); err != nil {
		t.Fatalf("disabled DeleteTuple should not error: %v", err)
	}
	tuples, err := c.ListTuples(context.Background(), TupleQuery{})
	if err != nil {
		t.Fatalf("disabled ListTuples should not error: %v", err)
	}
	if tuples != nil {
		t.Fatal("disabled ListTuples should return nil")
	}
}

func TestCheck_Allowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/relation-tuples/check" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}

		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["namespace"] != "workspace" {
			t.Errorf("namespace = %q, want workspace", body["namespace"])
		}
		if body["object"] != "ws-123" {
			t.Errorf("object = %q, want ws-123", body["object"])
		}
		if body["relation"] != "write" {
			t.Errorf("relation = %q, want write", body["relation"])
		}
		if body["subject_id"] != "user-456" {
			t.Errorf("subject_id = %q, want user-456", body["subject_id"])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	allowed, err := c.Check(context.Background(), CheckParams{
		Namespace: "workspace", Object: "ws-123", Relation: "write", SubjectID: "user-456",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("expected allowed=true")
	}
}

func TestCheck_Denied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"allowed": false})
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	allowed, err := c.Check(context.Background(), CheckParams{
		Namespace: "workspace", Object: "ws-1", Relation: "manage", SubjectID: "u1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("expected allowed=false")
	}
}

func TestCheck_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	_, err := c.Check(context.Background(), CheckParams{
		Namespace: "workspace", Object: "ws-1", Relation: "view", SubjectID: "u1",
	})
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestCheckWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)

		// Owner passes view, write, manage. Member passes view, write.
		// Viewer passes view only.
		allowed := false
		switch body["relation"] {
		case "view":
			allowed = true
		case "write":
			allowed = body["subject_id"] != "viewer-user"
		case "manage":
			allowed = body["subject_id"] == "owner-user"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"allowed": allowed})
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	ctx := context.Background()

	tests := []struct {
		name      string
		userID    string
		relation  string
		wantAllow bool
	}{
		{"owner view", "owner-user", "view", true},
		{"owner write", "owner-user", "write", true},
		{"owner manage", "owner-user", "manage", true},
		{"member view", "member-user", "view", true},
		{"member write", "member-user", "write", true},
		{"member manage", "member-user", "manage", false},
		{"viewer view", "viewer-user", "view", true},
		{"viewer write", "viewer-user", "write", false},
		{"viewer manage", "viewer-user", "manage", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.CheckWorkspace(ctx, "ws-1", tt.userID, tt.relation)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantAllow {
				t.Errorf("CheckWorkspace(%q, %q) = %v, want %v", tt.userID, tt.relation, got, tt.wantAllow)
			}
		})
	}
}

func TestWriteTuple(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/admin/relation-tuples" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}

		var body Tuple
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Namespace != "workspace" {
			t.Errorf("namespace = %q", body.Namespace)
		}
		if body.Object != "ws-new" {
			t.Errorf("object = %q", body.Object)
		}
		if body.Relation != "owners" {
			t.Errorf("relation = %q", body.Relation)
		}
		if body.SubjectID != "creator-user" {
			t.Errorf("subject_id = %q", body.SubjectID)
		}

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	err := c.WriteTuple(context.Background(), Tuple{
		Namespace: "workspace", Object: "ws-new", Relation: "owners", SubjectID: "creator-user",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteTuple(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		q := r.URL.Query()
		if q.Get("namespace") != "workspace" {
			t.Errorf("namespace = %q", q.Get("namespace"))
		}
		if q.Get("object") != "ws-1" {
			t.Errorf("object = %q", q.Get("object"))
		}
		if q.Get("relation") != "members" {
			t.Errorf("relation = %q", q.Get("relation"))
		}
		if q.Get("subject_id") != "removed-user" {
			t.Errorf("subject_id = %q", q.Get("subject_id"))
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	err := c.DeleteTuple(context.Background(), Tuple{
		Namespace: "workspace", Object: "ws-1", Relation: "members", SubjectID: "removed-user",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListTuples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/relation-tuples" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("namespace") != "workspace" {
			t.Errorf("namespace = %q", q.Get("namespace"))
		}
		if q.Get("object") != "ws-1" {
			t.Errorf("object = %q", q.Get("object"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"relation_tuples": []map[string]string{
				{"namespace": "workspace", "object": "ws-1", "relation": "owners", "subject_id": "u1"},
				{"namespace": "workspace", "object": "ws-1", "relation": "members", "subject_id": "u2"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	tuples, err := c.ListTuples(context.Background(), TupleQuery{
		Namespace: "workspace", Object: "ws-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tuples) != 2 {
		t.Fatalf("got %d tuples, want 2", len(tuples))
	}
	if tuples[0].Relation != "owners" || tuples[0].SubjectID != "u1" {
		t.Errorf("tuple[0] = %+v", tuples[0])
	}
	if tuples[1].Relation != "members" || tuples[1].SubjectID != "u2" {
		t.Errorf("tuple[1] = %+v", tuples[1])
	}
}

func TestCheck_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Check(ctx, CheckParams{
		Namespace: "workspace", Object: "ws-1", Relation: "view", SubjectID: "u1",
	})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// --- ListWorkspacesForUser (Phase 2 reverse-lookup helper) ---

func TestListWorkspacesForUser_Disabled(t *testing.T) {
	c := New("", "")
	got, err := c.ListWorkspacesForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %v", got)
	}
}

func TestListWorkspacesForUser_EmptyUserID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be hit for empty userID")
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	got, err := c.ListWorkspacesForUser(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %v", got)
	}
}

func TestListWorkspacesForUser_Dedup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rel := r.URL.Query().Get("relation")
		// Same workspace appears across multiple relations.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"relation_tuples": []map[string]string{
				{"namespace": "workspace", "object": "ws-1", "relation": rel, "subject_id": "u1"},
				{"namespace": "workspace", "object": "ws-2", "relation": rel, "subject_id": "u1"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	got, err := c.ListWorkspacesForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 unique workspaces, got %d: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, w := range got {
		if seen[w] {
			t.Fatalf("duplicate workspace: %s", w)
		}
		seen[w] = true
	}
}

func TestListWorkspacesForUser_Pagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token := r.URL.Query().Get("page_token")
		rel := r.URL.Query().Get("relation")
		switch token {
		case "":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"relation_tuples": []map[string]string{
					{"namespace": "workspace", "object": "ws-1", "relation": rel, "subject_id": "u1"},
				},
				"next_page_token": "tok-page2",
			})
		case "tok-page2":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"relation_tuples": []map[string]string{
					{"namespace": "workspace", "object": "ws-2", "relation": rel, "subject_id": "u1"},
				},
				// No next_page_token = last page.
			})
		default:
			t.Errorf("unexpected page_token: %s", token)
			http.Error(w, "unexpected token", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	got, err := c.ListWorkspacesForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 workspaces after pagination, got %d: %v", len(got), got)
	}
}

func TestListWorkspacesForUser_PaginationCap(t *testing.T) {
	// Always returns next_page_token to force the cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"relation_tuples": []map[string]string{},
			"next_page_token":  "still-going",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := c.ListWorkspacesForUser(ctx, "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty after cap, got %d", len(got))
	}
}

func TestListWorkspacesForUser_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "keto oops", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, srv.URL)
	_, err := c.ListWorkspacesForUser(context.Background(), "u1")
	if err == nil {
		t.Fatal("expected error on server error")
	}
}

func TestListWorkspacesForUser_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be hit when context is already cancelled")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := New(srv.URL, srv.URL)
	_, err := c.ListWorkspacesForUser(ctx, "u1")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}
