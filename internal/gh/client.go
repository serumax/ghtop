// Package gh reads GitHub Actions state through the authenticated `gh` CLI,
// so ghtop never handles tokens itself.
package gh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client runs `gh api` calls. Run is replaceable in tests.
type Client struct {
	Run func(args ...string) (body []byte, headers map[string]string, err error)

	mu   sync.Mutex
	last Limits
}

// New returns a Client backed by the real `gh` binary.
func New() *Client {
	return &Client{Run: ghRun}
}

// Repo is the subset of a repository ghtop needs.
type Repo struct {
	FullName string    `json:"full_name"`
	Archived bool      `json:"archived"`
	PushedAt time.Time `json:"pushed_at"`
}

// Run is a single workflow run.
type Run struct {
	Repo         string    `json:"-"`
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	RunNumber    int       `json:"run_number"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	Event        string    `json:"event"`
	HeadBranch   string    `json:"head_branch"`
	HTMLURL      string    `json:"html_url"`
	RunStartedAt time.Time `json:"run_started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Actor        struct {
		Login string `json:"login"`
	} `json:"actor"`
}

// Key identifies a run across repositories.
func (r Run) Key() string { return r.Repo + "#" + strconv.FormatInt(r.ID, 10) }

// Step is one step inside a job.
type Step struct {
	Number     int    `json:"number"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Job is one job of a run, with its steps.
type Job struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Steps       []Step    `json:"steps"`
}

// Limits reports the remaining REST quota seen on the last response.
type Limits struct {
	Remaining int
	Reset     time.Time
}

// Orgs lists the organizations the authenticated user belongs to.
func (c *Client) Orgs() ([]string, error) {
	var raw []struct {
		Login string `json:"login"`
	}
	if _, err := c.get("user/orgs?per_page=100", &raw); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, o := range raw {
		out = append(out, o.Login)
	}
	return out, nil
}

// Repos lists repositories for an org, or the user's own when org is "".
func (c *Client) Repos(org string) ([]Repo, error) {
	base := "user/repos?affiliation=owner&per_page=100"
	if org != "" {
		base = "orgs/" + org + "/repos?type=all&per_page=100"
	}
	var all []Repo
	for page := 1; ; page++ {
		var batch []Repo
		if _, err := c.get(base+"&page="+strconv.Itoa(page), &batch); err != nil {
			return all, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	active := all[:0]
	for _, r := range all {
		if !r.Archived {
			active = append(active, r)
		}
	}
	return active, nil
}

// Runs returns recent runs of a repo, optionally filtered by status
// (comma separated, e.g. "in_progress,queued").
func (c *Client) Runs(repo, status string, perPage int) ([]Run, error) {
	path := fmt.Sprintf("repos/%s/actions/runs?per_page=%d", repo, perPage)
	if status != "" {
		path += "&status=" + status
	}
	var resp struct {
		Runs []Run `json:"workflow_runs"`
	}
	if _, err := c.get(path, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Runs {
		resp.Runs[i].Repo = repo
	}
	return resp.Runs, nil
}

// Jobs returns the jobs (with steps) of one run.
func (c *Client) Jobs(repo string, runID int64) ([]Job, error) {
	var resp struct {
		Jobs []Job `json:"jobs"`
	}
	path := fmt.Sprintf("repos/%s/actions/runs/%d/jobs?per_page=100", repo, runID)
	if _, err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// JobLog returns the plain-text log of a job. GitHub only serves logs of
// finished jobs for most runs; callers should show the error otherwise.
func (c *Client) JobLog(repo string, jobID int64) (string, error) {
	path := fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repo, jobID)
	body, _, err := c.Run("api", path)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Rerun asks GitHub to re-run a whole workflow run.
func (c *Client) Rerun(repo string, runID int64) error {
	path := fmt.Sprintf("repos/%s/actions/runs/%d/rerun", repo, runID)
	_, _, err := c.Run("api", "-X", "POST", path)
	return err
}

// Limit returns the rate-limit state seen on the most recent JSON response.
func (c *Client) Limit() Limits {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *Client) get(path string, out any) (Limits, error) {
	body, headers, err := c.Run("api", path)
	if err != nil {
		return Limits{}, err
	}
	lim := parseLimits(headers)
	c.mu.Lock()
	c.last = lim
	c.mu.Unlock()
	if err := json.Unmarshal(body, out); err != nil {
		return lim, fmt.Errorf("decode %s: %w", path, err)
	}
	return lim, nil
}

func parseLimits(h map[string]string) Limits {
	var l Limits
	if v, ok := h["x-ratelimit-remaining"]; ok {
		l.Remaining, _ = strconv.Atoi(v)
	}
	if v, ok := h["x-ratelimit-reset"]; ok {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			l.Reset = time.Unix(sec, 0)
		}
	}
	return l
}

// ghRun calls `gh api -i` so response headers (rate limit) come back with the body.
func ghRun(args ...string) ([]byte, map[string]string, error) {
	full := append([]string{"api", "-i"}, args[1:]...)
	cmd := exec.Command("gh", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, nil, fmt.Errorf("%s", msg)
	}
	headers, body := splitResponse(stdout.Bytes())
	return body, headers, nil
}

// splitResponse separates the header block from the body of `gh api -i`.
func splitResponse(raw []byte) (map[string]string, []byte) {
	headers := map[string]string{}
	text := string(raw)
	idx := strings.Index(text, "\r\n\r\n")
	sep := 4
	if idx < 0 {
		idx = strings.Index(text, "\n\n")
		sep = 2
	}
	if idx < 0 {
		return headers, raw
	}
	for _, line := range strings.Split(text[:idx], "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return headers, []byte(text[idx+sep:])
}
