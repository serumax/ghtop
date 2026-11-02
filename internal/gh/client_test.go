package gh

import (
	"testing"
	"time"
)

func TestSplitResponseSeparatesHeadersAndBody(t *testing.T) {
	raw := []byte("HTTP/2.0 200 OK\r\nX-RateLimit-Remaining: 4931\r\n\r\n{\"ok\":true}")
	h, body := splitResponse(raw)
	if h["x-ratelimit-remaining"] != "4931" {
		t.Fatalf("remaining header = %q", h["x-ratelimit-remaining"])
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

func TestRunsDecodesWorkflowRunsAndTagsRepo(t *testing.T) {
	c := &Client{Run: func(args ...string) ([]byte, map[string]string, error) {
		return []byte(`{"workflow_runs":[{"id":42,"name":"deploy","status":"in_progress","conclusion":null,"run_started_at":"2026-10-28T10:00:00Z"}]}`),
			map[string]string{"x-ratelimit-remaining": "100"}, nil
	}}
	runs, err := c.Runs("ida/web", "in_progress", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Key() != "ida/web#42" || runs[0].Name != "deploy" {
		t.Fatalf("unexpected runs: %+v", runs)
	}
	if c.Limit().Remaining != 100 {
		t.Fatalf("limit not tracked: %+v", c.Limit())
	}
	if runs[0].RunStartedAt.IsZero() || runs[0].RunStartedAt.After(time.Now()) {
		t.Fatalf("bad start time: %v", runs[0].RunStartedAt)
	}
}
