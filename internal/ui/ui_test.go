package ui

import (
	"testing"
	"time"

	"ghtop/internal/gh"
)

func TestWatchSetKeepsRecentPushesNewestFirstAndCaps(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	repos := []gh.Repo{
		{FullName: "ida/old", PushedAt: now.Add(-200 * time.Hour)},
		{FullName: "ida/a", PushedAt: now.Add(-1 * time.Hour)},
		{FullName: "ida-cclm/b", PushedAt: now.Add(-2 * time.Hour)},
		{FullName: "ida/c", PushedAt: now.Add(-3 * time.Hour)},
	}
	got := watchSet(repos, 2, now)
	want := []string{"ida/a", "ida-cclm/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("watchSet = %v, want %v", got, want)
	}
}

func TestVisibleFiltersByModeAndQuery(t *testing.T) {
	m := New(nil, Options{})
	m.runs = map[string]gh.Run{
		"ida/web#1": {Repo: "ida/web", ID: 1, Name: "deploy", Status: "in_progress"},
		"ida/api#2": {Repo: "ida/api", ID: 2, Name: "ci", Status: "completed", Conclusion: "failure"},
		"ida/api#3": {Repo: "ida/api", ID: 3, Name: "ci", Status: "completed", Conclusion: "success"},
	}
	m.order = []string{"ida/web#1", "ida/api#2", "ida/api#3"}

	if n := len(m.visible()); n != 3 {
		t.Fatalf("all mode visible = %d", n)
	}
	m.mode = 1 // active
	if n := len(m.visible()); n != 1 {
		t.Fatalf("active mode visible = %d", n)
	}
	m.mode = 2 // failed
	if n := len(m.visible()); n != 1 {
		t.Fatalf("failed mode visible = %d", n)
	}
	m.mode = 0
	m.filter = "WEB"
	if n := len(m.visible()); n != 1 {
		t.Fatalf("query visible = %d", n)
	}
}
