// Command ghtop is a live, htop-style view of GitHub Actions runs across
// every organization and repository you can reach through `gh`.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ghtop/internal/gh"
	"ghtop/internal/ui"
)

func main() {
	orgs := flag.String("orgs", "", "comma-separated orgs to watch (default: every org you belong to)")
	interval := flag.Duration("interval", 30*time.Second, "sweep interval for watched repos")
	maxRepos := flag.Int("max-repos", 40, "max repos swept per cycle (most recently pushed first)")
	repoRefresh := flag.Duration("repo-refresh", 15*time.Minute, "how often the repo list is reloaded")
	flag.Parse()

	if _, err := os.Stat(os.Getenv("HOME")); err != nil {
		fmt.Fprintln(os.Stderr, "ghtop:", err)
		os.Exit(1)
	}

	var only []string
	for _, o := range strings.Split(*orgs, ",") {
		if o = strings.TrimSpace(o); o != "" {
			only = append(only, o)
		}
	}

	opts := ui.Options{
		Orgs:         only,
		Interval:     *interval,
		MaxRepos:     *maxRepos,
		RepoInterval: *repoRefresh,
	}
	p := tea.NewProgram(ui.New(gh.New(), opts), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ghtop:", err)
		os.Exit(1)
	}
}
