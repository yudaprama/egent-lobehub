// Package authz provides a client for Ory Keto's relationship-tuple API.
// It replaces the SQL-based RBAC system (rbac_roles, rbac_permissions,
// rbac_role_permissions, rbac_user_roles) with Zanzibar-style checks.
//
// The workspace namespace defines three relations: owners, members, viewers.
// Permission tiers are: view (viewer+), write (member+), manage (owner only).
// The :all vs :owner scope distinction stays in the application layer.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrNotConfigured = errors.New("authz: Keto not configured")
	ErrKetoUnhealthy = errors.New("authz: Keto request failed")
)

// Tuple is a relationship tuple in Keto's Zanzibar model.
type Tuple struct {
	Namespace string `json:"namespace"`
	Object    string `json:"object"`
	Relation  string `json:"relation"`
	SubjectID string `json:"subject_id"`
}

// CheckParams specifies a permission check query.
type CheckParams struct {
	Namespace string
	Object    string
	Relation  string
	SubjectID string
}

// TupleQuery filters tuples for listing.
type TupleQuery struct {
	Namespace string
	Object    string
	Relation  string
	SubjectID string
}

// Client talks to Ory Keto's read and write APIs.
type Client struct {
	readURL    string
	writeURL   string
	httpClient *http.Client
}

// New creates a Keto client. Pass empty strings to disable (Check always
// returns true, writes are no-ops) — useful for personal-scope deployments.
func New(readURL, writeURL string) *Client {
	return &Client{
		readURL:  readURL,
		writeURL: writeURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Enabled reports whether the client has Keto URLs configured.
func (c *Client) Enabled() bool {
	return c.readURL != "" && c.writeURL != ""
}

// Check answers: does subject have relation on namespace:object?
// Returns true when Keto is not configured (fail-open for personal scope).
func (c *Client) Check(ctx context.Context, params CheckParams) (bool, error) {
	if !c.Enabled() {
		return true, nil
	}

	body := map[string]string{
		"namespace":  params.Namespace,
		"object":     params.Object,
		"relation":   params.Relation,
		"subject_id": params.SubjectID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("authz: marshal check body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.readURL+"/relation-tuples/check", bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Errorf("authz: build check request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrKetoUnhealthy, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("%w: status %d: %s", ErrKetoUnhealthy, resp.StatusCode, respBody)
	}

	var result struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("authz: decode check response: %w", err)
	}
	return result.Allowed, nil
}

// CheckWorkspace is a convenience wrapper for the workspace namespace.
// relation should be "view", "write", or "manage".
func (c *Client) CheckWorkspace(ctx context.Context, workspaceID, userID, relation string) (bool, error) {
	return c.Check(ctx, CheckParams{
		Namespace: "workspace",
		Object:    workspaceID,
		Relation:  relation,
		SubjectID: userID,
	})
}

// WriteTuple creates or updates a relationship tuple (idempotent PUT).
// No-op when Keto is not configured.
func (c *Client) WriteTuple(ctx context.Context, tuple Tuple) error {
	if !c.Enabled() {
		return nil
	}

	payload, err := json.Marshal(tuple)
	if err != nil {
		return fmt.Errorf("authz: marshal tuple: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.writeURL+"/admin/relation-tuples", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("authz: build write request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKetoUnhealthy, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: status %d: %s", ErrKetoUnhealthy, resp.StatusCode, respBody)
	}
	return nil
}

// DeleteTuple removes a relationship tuple.
// No-op when Keto is not configured.
func (c *Client) DeleteTuple(ctx context.Context, tuple Tuple) error {
	if !c.Enabled() {
		return nil
	}

	params := url.Values{}
	params.Set("namespace", tuple.Namespace)
	params.Set("object", tuple.Object)
	params.Set("relation", tuple.Relation)
	params.Set("subject_id", tuple.SubjectID)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.writeURL+"/admin/relation-tuples?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("authz: build delete request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrKetoUnhealthy, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: status %d: %s", ErrKetoUnhealthy, resp.StatusCode, respBody)
	}
	return nil
}

// ListTuples returns all tuples matching the query. Pagination is not
// implemented — returns the first page (up to Keto's default limit).
func (c *Client) ListTuples(ctx context.Context, query TupleQuery) ([]Tuple, error) {
	if !c.Enabled() {
		return nil, nil
	}

	params := url.Values{}
	if query.Namespace != "" {
		params.Set("namespace", query.Namespace)
	}
	if query.Object != "" {
		params.Set("object", query.Object)
	}
	if query.Relation != "" {
		params.Set("relation", query.Relation)
	}
	if query.SubjectID != "" {
		params.Set("subject_id", query.SubjectID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.readURL+"/relation-tuples?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("authz: build list request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKetoUnhealthy, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: status %d: %s", ErrKetoUnhealthy, resp.StatusCode, respBody)
	}

	var result struct {
		Tuples []struct {
			Namespace string `json:"namespace"`
			Object    string `json:"object"`
			Relation  string `json:"relation"`
			SubjectID string `json:"subject_id"`
		} `json:"relation_tuples"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("authz: decode list response: %w", err)
	}

	tuples := make([]Tuple, len(result.Tuples))
	for i, t := range result.Tuples {
		tuples[i] = Tuple{
			Namespace: t.Namespace,
			Object:    t.Object,
			Relation:  t.Relation,
			SubjectID: t.SubjectID,
		}
	}
	return tuples, nil
}

// ListWorkspacesForUser returns the deduplicated set of workspace IDs
// that `userID` belongs to (owners ∪ members ∪ viewers) in the
// `workspace` namespace. Handles Keto pagination (page_token) up to
// maxPages (10) per relation; beyond that it returns what it has.
//
// This is the reverse-lookup helper used by the pREST
// WorkspaceMembershipResolver middleware (Phase 2 of the workspace
// scope implementation). The dedicated `prest/internal/keto` package
// mirrors this method to avoid a Go module dependency on egent-lobehub
// from pREST — both stay in sync with Keto's wire format.
//
// Returns ([]string{}, nil) when Keto is not configured (fail-open for
// personal-scope deployments). Returns (partial, err) on transport or
// HTTP errors so callers can decide fail-open vs fail-closed.
func (c *Client) ListWorkspacesForUser(ctx context.Context, userID string) ([]string, error) {
	if !c.Enabled() {
		return []string{}, nil
	}
	if userID == "" {
		return []string{}, nil
	}

	const maxPages = 10
	relations := []string{"owners", "members", "viewers"}
	seen := make(map[string]struct{})
	result := make([]string, 0)

	for _, rel := range relations {
		pageToken := ""
		pages := 0
		for {
			pages++
			if pages > maxPages {
				break
			}

			params := url.Values{}
			params.Set("namespace", "workspace")
			params.Set("relation", rel)
			params.Set("subject_id", userID)
			if pageToken != "" {
				params.Set("page_token", pageToken)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				c.readURL+"/relation-tuples?"+params.Encode(), nil)
			if err != nil {
				return result, fmt.Errorf("authz: build list request: %w", err)
			}

			resp, err := c.httpClient.Do(req)
			if err != nil {
				return result, fmt.Errorf("%w: %v", ErrKetoUnhealthy, err)
			}

			if resp.StatusCode != http.StatusOK {
				respBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return result, fmt.Errorf("%w: status %d: %s", ErrKetoUnhealthy, resp.StatusCode, respBody)
			}

			var page struct {
				Tuples []struct {
					Object string `json:"object"`
				} `json:"relation_tuples"`
				NextPageToken string `json:"next_page_token"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
				resp.Body.Close()
				return result, fmt.Errorf("authz: decode list response: %w", err)
			}
			resp.Body.Close()

			for _, t := range page.Tuples {
				if t.Object == "" {
					continue
				}
				if _, ok := seen[t.Object]; ok {
					continue
				}
				seen[t.Object] = struct{}{}
				result = append(result, t.Object)
			}

			pageToken = page.NextPageToken
			if pageToken == "" {
				break
			}
		}
	}

	return result, nil
}
