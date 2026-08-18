// Package github opens pull requests.
//
// Deliberately tiny: Doot needs exactly one endpoint. A full API client would be
// a dependency and a surface area for no benefit, since the work of getting code
// onto the branch is done by git inside the sandbox.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.github.com"

// Client talks to the GitHub REST API.
type Client struct {
	token string
	http  *http.Client
}

// New builds a Client.
func New(token string) *Client {
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// PullRequestInput describes the pull request to open.
type PullRequestInput struct {
	Owner string
	Repo  string
	Head  string
	Base  string
	Title string
	Body  string
}

// PullRequest is an opened or existing pull request.
type PullRequest struct {
	Number int
	URL    string

	// AlreadyExisted reports that a pull request for this branch was already
	// open. This is the normal case for the second and later pushes to the same
	// branch, not an error.
	AlreadyExisted bool
}

// CreatePullRequest opens a pull request, or finds the existing one.
func (c *Client) CreatePullRequest(ctx context.Context, in PullRequestInput) (PullRequest, error) {
	if c.token == "" {
		return PullRequest{}, errors.New("github: no token configured")
	}
	if in.Owner == "" || in.Repo == "" || in.Head == "" || in.Base == "" {
		return PullRequest{}, errors.New("github: owner, repo, head and base are all required")
	}
	if strings.TrimSpace(in.Title) == "" {
		in.Title = "Doot changes"
	}

	payload := map[string]any{
		"title": in.Title,
		"head":  in.Head,
		"base":  in.Base,
		"body":  in.Body,
	}

	status, respBody, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/pulls", in.Owner, in.Repo), payload)
	if err != nil {
		return PullRequest{}, err
	}

	switch {
	case status == http.StatusCreated:
		var created struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(respBody, &created); err != nil {
			return PullRequest{}, fmt.Errorf("github: decode created pull request: %w", err)
		}
		return PullRequest{Number: created.Number, URL: created.HTMLURL}, nil

	case status == http.StatusUnprocessableEntity:
		// GitHub returns 422 both for "a pull request already exists" and for
		// genuine validation problems such as no commits between branches. The
		// first is expected, so look for the existing PR before treating it as a
		// failure.
		if existing, found, findErr := c.findOpenPullRequest(ctx, in); findErr == nil && found {
			existing.AlreadyExisted = true
			return existing, nil
		}
		return PullRequest{}, fmt.Errorf("github: pull request rejected: %s", apiMessage(respBody))

	default:
		return PullRequest{}, fmt.Errorf("github: pull request failed (status %d): %s",
			status, apiMessage(respBody))
	}
}

// findOpenPullRequest looks for an open pull request from the head branch.
func (c *Client) findOpenPullRequest(ctx context.Context, in PullRequestInput) (PullRequest, bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&head=%s:%s&base=%s",
		in.Owner, in.Repo, in.Owner, in.Head, in.Base)

	status, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return PullRequest{}, false, err
	}
	if status != http.StatusOK {
		return PullRequest{}, false, fmt.Errorf("github: list pull requests failed (status %d)", status)
	}

	var list []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return PullRequest{}, false, fmt.Errorf("github: decode pull request list: %w", err)
	}
	if len(list) == 0 {
		return PullRequest{}, false, nil
	}

	return PullRequest{Number: list[0].Number, URL: list[0].HTMLURL}, true, nil
}

// do performs one API request.
func (c *Client) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("github: encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("github: build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "doot")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("github: request failed: %w", err)
	}
	defer resp.Body.Close()

	// Capped: an unexpected HTML error page should not be read into memory
	// wholesale.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("github: read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// apiMessage extracts a readable message from an error response.
func apiMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Field   string `json:"field"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return strings.TrimSpace(truncate(string(body), 300))
	}

	parts := []string{}
	if payload.Message != "" {
		parts = append(parts, payload.Message)
	}
	for _, e := range payload.Errors {
		switch {
		case e.Message != "":
			parts = append(parts, e.Message)
		case e.Field != "":
			parts = append(parts, fmt.Sprintf("%s %s", e.Field, e.Code))
		}
	}
	if len(parts) == 0 {
		return "no message"
	}
	return strings.Join(parts, "; ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
