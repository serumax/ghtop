// Package ui renders the ghtop TUI: a k9s/htop-style live view of GitHub
// Actions runs across every watched repository.
package ui

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ghtop/internal/gh"
)

// Options configure polling and scope.
type Options struct {
	Orgs         []string      // empty means every org the user belongs to
	Interval     time.Duration // sweep interval for watched repos
	MaxRepos     int           // cap on repos swept each cycle
	RepoInterval time.Duration // how often the repo list is refreshed
}

type viewKind int

const (
	viewList viewKind = iota
	viewDetail
	viewLog
)

var modes = []string{"all", "active", "failed"}

type detailRow struct {
	job  int // index into jobs
	step int // index into jobs[job].Steps, -1 for the job row
}

type (
	reposMsg struct {
		repos []gh.Repo
		orgs  []string
		err   error
	}
	sweepMsg struct {
		runs []gh.Run
		errs map[string]string
		lim  gh.Limits
	}
	sweepTick struct{}
	repoTick  struct{}
	jobsMsg   struct {
		key  string
		jobs []gh.Job
		err  error
	}
	logMsg struct {
		title string
		text  string
		err   error
	}
	rerunMsg struct {
		key string
		err error
	}
)

// Model is the Bubble Tea model.
type Model struct {
	c    *gh.Client
	opts Options

	orgs      []string
	repos     []gh.Repo
	watch     []string
	runs      map[string]gh.Run
	order     []string
	errs      map[string]string
	lim       gh.Limits
	busy      bool
	chained   bool
	lastSweep time.Time

	view     viewKind
	cursor   int
	mode     int
	filter   string
	filtLanging bool
	confirm  string // run key awaiting rerun confirmation

	jobsKey string
	jobs    []gh.Job
	jobErr  error
	rows    []detailRow
	dcursor int

	logTitle string
	logLines []string
	logOff   int
	logErr   error
	loading  bool

	status        string
	width, height int
}

// New builds the initial model. Call Init to start loading.
func New(c *gh.Client, opts Options) Model {
	return Model{
		c:    c,
		opts: opts,
		runs: map[string]gh.Run{},
		errs: map[string]string{},
	}
}

// Init loads the repository inventory.
func (m Model) Init() tea.Cmd {
	return loadRepos(m.c, m.opts.Orgs)
}

func loadRepos(c *gh.Client, only []string) tea.Cmd {
	return func() tea.Msg {
		var orgs []string
		var err error
		if len(only) > 0 {
			orgs = only
		} else if orgs, err = c.Orgs(); err != nil {
			return reposMsg{err: err}
		}
		seen := map[string]gh.Repo{}
		var firstErr error
		fetch := func(org string) {
			rs, e := c.Repos(org)
			if e != nil && firstErr == nil {
				firstErr = e
			}
			for _, r := range rs {
				seen[r.FullName] = r
			}
		}
		fetch("") // personal repos
		for _, o := range orgs {
			fetch(o)
		}
		repos := make([]gh.Repo, 0, len(seen))
		for _, r := range seen {
			repos = append(repos, r)
		}
		return reposMsg{repos: repos, orgs: orgs, err: firstErr}
	}
}

// watchSet picks the repos worth sweeping: most recently pushed first.
func watchSet(repos []gh.Repo, max int, now time.Time) []string {
	cutoff := now.Add(-72 * time.Hour)
	recent := make([]gh.Repo, 0, len(repos))
	for _, r := range repos {
		if r.PushedAt.After(cutoff) {
			recent = append(recent, r)
		}
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].PushedAt.After(recent[j].PushedAt) })
	if max > 0 && len(recent) > max {
		recent = recent[:max]
	}
	out := make([]string, 0, len(recent))
	for _, r := range recent {
		out = append(out, r.FullName)
	}
	return out
}

func sweep(c *gh.Client, repos []string) tea.Cmd {
	return func() tea.Msg {
		var (
			mu   sync.Mutex
			wg   sync.WaitGroup
			sem  = make(chan struct{}, 8)
			runs []gh.Run
			errs = map[string]string{}
		)
		for _, r := range repos {
			wg.Add(1)
			go func(repo string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				rs, err := c.Runs(repo, "", 5)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs[repo] = err.Error()
					return
				}
				runs = append(runs, rs...)
			}(r)
		}
		wg.Wait()
		return sweepMsg{runs: runs, errs: errs, lim: c.Limit()}
	}
}

func (m *Model) scheduleSweep() tea.Cmd {
	wait := m.opts.Interval
	if m.lim.Remaining > 0 && m.lim.Remaining < 500 {
		wait *= 4 // back off when the REST quota runs low
	}
	return tea.Tick(wait, func(time.Time) tea.Msg { return sweepTick{} })
}

func fetchJobs(c *gh.Client, repo string, id int64, key string) tea.Cmd {
	return func() tea.Msg {
		jobs, err := c.Jobs(repo, id)
		return jobsMsg{key: key, jobs: jobs, err: err}
	}
}

func fetchLog(c *gh.Client, repo string, job gh.Job) tea.Cmd {
	return func() tea.Msg {
		text, err := c.JobLog(repo, job.ID)
		return logMsg{title: job.Name, text: text, err: err}
	}
}

func rerun(c *gh.Client, run gh.Run) tea.Cmd {
	return func() tea.Msg {
		return rerunMsg{key: run.Key(), err: c.Rerun(run.Repo, run.ID)}
	}
}

// Update handles messages and keys.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case reposMsg:
		if msg.err != nil {
			m.status = "repos: " + msg.err.Error()
		}
		m.orgs = msg.orgs
		m.repos = msg.repos
		m.watch = watchSet(msg.repos, m.opts.MaxRepos, time.Now())
		cmds := []tea.Cmd{tea.Tick(m.opts.RepoInterval, func(time.Time) tea.Msg { return repoTick{} })}
		if !m.chained {
			m.chained = true
			m.busy = true
			cmds = append(cmds, sweep(m.c, m.watch))
		}
		return m, tea.Batch(cmds...)

	case repoTick:
		return m, loadRepos(m.c, m.opts.Orgs)

	case sweepTick:
		if m.busy {
			return m, m.scheduleSweep()
		}
		m.busy = true
		return m, sweep(m.c, m.watch)

	case sweepMsg:
		m.busy = false
		m.lastSweep = time.Now()
		m.lim = msg.lim
		m.errs = msg.errs
		swept := map[string]bool{}
		for _, r := range m.watch {
			swept[r] = true
		}
		// Replace the swept repos' runs with the fresh window.
		for k, r := range m.runs {
			if swept[r.Repo] {
				delete(m.runs, k)
			}
		}
		for _, r := range msg.runs {
			m.runs[r.Key()] = r
		}
		m.reorder()
		return m, m.scheduleSweep()

	case jobsMsg:
		if msg.key != m.jobsKey {
			return m, nil
		}
		m.loading = false
		m.jobs, m.jobErr = msg.jobs, msg.err
		m.rebuildRows()
		return m, nil

	case logMsg:
		m.loading = false
		m.logTitle = msg.title
		m.logErr = msg.err
		m.logLines = strings.Split(strings.TrimRight(msg.text, "\n"), "\n")
		m.logOff = 0
		m.view = viewLog
		return m, nil

	case rerunMsg:
		if msg.err != nil {
			m.status = "rerun failed: " + msg.err.Error()
		} else {
			m.status = "rerun requested for " + msg.key
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) reorder() {
	m.order = m.order[:0]
	for k := range m.runs {
		m.order = append(m.order, k)
	}
	sort.Slice(m.order, func(i, j int) bool {
		a, b := m.runs[m.order[i]], m.runs[m.order[j]]
		ea, eb := isActive(a), isActive(b)
		if ea != eb {
			return ea
		}
		if ea {
			return a.RunStartedAt.After(b.RunStartedAt)
		}
		return a.UpdatedAt.After(b.UpdatedAt)
	})
	m.clampCursor()
}

func (m *Model) rebuildRows() {
	m.rows = m.rows[:0]
	for ji, j := range m.jobs {
		m.rows = append(m.rows, detailRow{job: ji, step: -1})
		for si := range j.Steps {
			m.rows = append(m.rows, detailRow{job: ji, step: si})
		}
	}
	if m.dcursor >= len(m.rows) {
		m.dcursor = 0
	}
}

func (m Model) visible() []string {
	var out []string
	for _, k := range m.order {
		r := m.runs[k]
		switch modes[m.mode] {
		case "active":
			if !isActive(r) {
				continue
			}
		case "failed":
			if !isFailed(r) {
				continue
			}
		}
		if m.filter != "" && !matches(r, m.filter) {
			continue
		}
		out = append(out, k)
	}
	return out
}

func (m *Model) clampCursor() {
	n := len(m.visible())
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m Model) selected() (gh.Run, bool) {
	v := m.visible()
	if m.cursor < 0 || m.cursor >= len(v) {
		return gh.Run{}, false
	}
	return m.runs[v[m.cursor]], true
}

func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, tea.Quit
	}
	if m.confirm != "" {
		if s == "y" {
			run := m.runs[m.confirm]
			m.confirm = ""
			m.status = "rerunning " + run.Key() + "..."
			return m, rerun(m.c, run)
		}
		m.confirm = ""
		m.status = "rerun cancelled"
		return m, nil
	}
	if m.filtLanging {
		switch s {
		case "esc":
			m.filter = ""
			m.filtLanging = false
		case "enter":
			m.filtLanging = false
		case "backspace":
			if len(m.filter) > 0 {
				r := []rune(m.filter)
				m.filter = string(r[:len(r)-1])
			}
		default:
			if k.Type == tea.KeyRunes {
				m.filter += string(k.Runes)
			}
		}
		m.cursor = 0
		m.clampCursor()
		return m, nil
	}
	switch m.view {
	case viewList:
		return m.keyList(s)
	case viewDetail:
		return m.keyDetail(s)
	case viewLog:
		return m.keyLog(s)
	}
	return m, nil
}


func (m Model) keyList(s string) (tea.Model, tea.Cmd) {
	n := len(m.visible())
	switch s {
	case "q":
		return m, tea.Quit
	case "j", "down":
		if m.cursor < n-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = n - 1
		m.clampCursor()
	case "f":
		m.mode = (m.mode + 1) % len(modes)
		m.cursor = 0
		m.clampCursor()
	case "/":
		m.filtLanging = true
	case "enter", "l":
		run, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.view = viewDetail
		m.jobsKey = run.Key()
		m.jobs, m.jobErr, m.rows, m.dcursor = nil, nil, nil, 0
		m.loading = true
		return m, fetchJobs(m.c, run.Repo, run.ID, run.Key())
	case "o":
		if run, ok := m.selected(); ok {
			_ = exec.Command("xdg-open", run.HTMLURL).Start()
		}
	case "R":
		if run, ok := m.selected(); ok && run.Status == "completed" {
			m.confirm = run.Key()
			m.status = fmt.Sprintf("re-run %s #%d? press y to confirm", run.Repo, run.RunNumber)
		} else {
			m.status = "only completed runs can be re-run"
		}
	}
	return m, nil
}

func (m Model) keyDetail(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "esc", "h", "backspace":
		m.view = viewList
	case "q":
		return m, tea.Quit
	case "j", "down":
		if m.dcursor < len(m.rows)-1 {
			m.dcursor++
		}
	case "k", "up":
		if m.dcursor > 0 {
			m.dcursor--
		}
	case "e": // jump to first failed step
		for i, r := range m.rows {
			if r.step >= 0 && m.jobs[r.job].Steps[r.step].Conclusion == "failure" {
				m.dcursor = i
				break
			}
		}
	case "enter", "l":
		if len(m.rows) == 0 {
			return m, nil
		}
		j := m.jobs[m.rows[m.dcursor].job]
		run, ok := m.runFor(m.jobsKey)
		if !ok {
			return m, nil
		}
		m.loading = true
		return m, fetchLog(m.c, run.Repo, j)
	case "R":
		if run, ok := m.runFor(m.jobsKey); ok && run.Status == "completed" {
			m.confirm = run.Key()
			m.status = fmt.Sprintf("re-run %s #%d? press y to confirm", run.Repo, run.RunNumber)
		}
	}
	return m, nil
}

func (m Model) runFor(key string) (gh.Run, bool) {
	r, ok := m.runs[key]
	return r, ok
}

func (m Model) keyLog(s string) (tea.Model, tea.Cmd) {
	page := m.bodyRows()
	switch s {
	case "esc", "h", "backspace":
		m.view = viewDetail
	case "q":
		return m, tea.Quit
	case "j", "down":
		m.logOff++
	case "k", "up":
		m.logOff--
	case "pgdown", " ":
		m.logOff += page
	case "pgup", "b":
		m.logOff -= page
	case "g":
		m.logOff = 0
	case "G":
		m.logOff = len(m.logLines)
	}
	if m.logOff > len(m.logLines)-1 {
		m.logOff = len(m.logLines) - 1
	}
	if m.logOff < 0 {
		m.logOff = 0
	}
	return m, nil
}

// bodyRows is how many content lines fit under the header and footer.
func (m Model) bodyRows() int {
	n := m.height - 5
	if n < 5 {
		n = 5
	}
	return n
}

// ---- styles & rendering ----

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selStyle   = lipgloss.NewStyle().Reverse(true)
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	runStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	queueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
)

func isActive(r gh.Run) bool {
	return r.Status != "completed"
}

func isFailed(r gh.Run) bool {
	switch r.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

func matches(r gh.Run, q string) bool {
	q = strings.ToLower(q)
	for _, f := range []string{r.Repo, r.Name, r.DisplayTitle, r.HeadBranch, r.Event, r.Actor.Login} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// badge returns the glyph, label and style for a status pair.
func badge(status, conclusion string) (string, string, lipgloss.Style) {
	switch status {
	case "in_progress":
		return "●", "running", runStyle
	case "queued", "waiting", "pending", "requested":
		return "◌", status, queueStyle
	}
	switch conclusion {
	case "success":
		return "✓", "success", okStyle
	case "failure", "timed_out", "startup_failure":
		return "✗", conclusion, failStyle
	case "cancelled":
		return "⊘", "cancelled", dimStyle
	case "skipped":
		return "–", "skipped", dimStyle
	case "action_required":
		return "!", "action req", queueStyle
	}
	return "·", conclusion, dimStyle
}

func elapsed(r gh.Run, now time.Time) string {
	start := r.RunStartedAt
	if start.IsZero() {
		start = r.UpdatedAt
	}
	end := r.UpdatedAt
	if isActive(r) {
		end = now
	}
	return dur(end.Sub(start))
}

func dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}

func ago(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return dur(now.Sub(t)) + " ago"
}

func clip(s string, n int) string {
	r := []rune(s)
	if n <= 0 {
		return ""
	}
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "…"
}

func pad(s string, n int) string {
	s = clip(s, n)
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// View renders the current screen.
func (m Model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	switch m.view {
	case viewDetail:
		return m.viewDetail()
	case viewLog:
		return m.viewLog()
	}
	return m.viewList()
}

func (m Model) header() string {
	now := time.Now()
	var active, failed int
	for _, r := range m.runs {
		if isActive(r) {
			active++
		}
		if isFailed(r) {
			failed++
		}
	}
	age := "never"
	if !m.lastSweep.IsZero() {
		age = dur(now.Sub(m.lastSweep)) + " ago"
	}
	rate := "?"
	if m.lim.Remaining > 0 {
		rate = fmt.Sprint(m.lim.Remaining)
	}
	line := fmt.Sprintf("ghtop · orgs %d · repos %d (watching %d) · active %d · failed %d · rate %s · swept %s",
		len(m.orgs), len(m.repos), len(m.watch), active, failed, rate, age)
	return titleStyle.Render(clip(line, m.width))
}

func (m Model) footer(keys string) string {
	var b strings.Builder
	if m.filtLanging {
		b.WriteString(queueStyle.Render("/" + m.filter + "▌"))
		b.WriteString("  " + dimStyle.Render("enter keep · esc clear"))
	} else {
		b.WriteString(dimStyle.Render(keys))
	}
	if m.status != "" {
		b.WriteString("\n" + dimStyle.Render(clip(m.status, m.width)))
	}
	for _, e := range m.sortedErrs() {
		b.WriteString("\n" + errStyle.Render(clip("! "+e, m.width)))
		break
	}
	return b.String()
}

func (m Model) sortedErrs() []string {
	out := make([]string, 0, len(m.errs))
	for repo, e := range m.errs {
		out = append(out, repo+": "+e)
	}
	sort.Strings(out)
	return out
}

func (m Model) viewList() string {
	now := time.Now()
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("filter: %s", modes[m.mode])))
	if m.filter != "" {
		b.WriteString(dimStyle.Render("  /" + m.filter))
	}
	b.WriteString("\n")

	cols := m.width - 4
	repoW := max(12, cols/4)
	wfW := max(12, cols/4)
	brW := max(8, cols/6)
	b.WriteString(dimStyle.Render(fmt.Sprintf("  %s %s %s %s %s",
		pad("STATUS", 10), pad("REPO", repoW), pad("WORKFLOW", wfW), pad("BRANCH", brW), "AGE / ELAPSED")) + "\n")

	v := m.visible()
	rows := m.bodyRows() - 1
	start := 0
	if m.cursor >= rows {
		start = m.cursor - rows + 1
	}
	if len(v) == 0 {
		b.WriteString(dimStyle.Render("  no runs yet" + waitHint(m)) + "\n")
	}
	for i := start; i < len(v) && i < start+rows; i++ {
		r := m.runs[v[i]]
		ic, label, st := badge(r.Status, r.Conclusion)
		line := fmt.Sprintf("%s %s %s %s %s",
			st.Render(pad(ic+" "+label, 10)),
			pad(r.Repo, repoW), pad(r.Name, wfW), pad(r.HeadBranch, brW),
			elapsed(r, now)+" · "+ago(r.UpdatedAt, now))
		if i == m.cursor {
			line = selStyle.Render(clip(stripANSI(line), m.width-2))
		}
		b.WriteString("  " + line + "\n")
	}
	b.WriteString(m.footer("j/k move · enter detail · f filter · / search · R rerun · o open · q quit"))
	return b.String()
}

func waitHint(m Model) string {
	if m.lastSweep.IsZero() {
		return " (loading…)"
	}
	return ""
}

func (m Model) viewDetail() string {
	run, ok := m.runFor(m.jobsKey)
	var b strings.Builder
	if !ok {
		return "run no longer available\n" + m.footer("esc back")
	}
	ic, label, st := badge(run.Status, run.Conclusion)
	b.WriteString(titleStyle.Render(clip(fmt.Sprintf("%s #%d · %s · %s", run.Repo, run.RunNumber, run.Name, run.DisplayTitle), m.width)) + "\n")
	b.WriteString(st.Render(ic+" "+label) + dimStyle.Render(fmt.Sprintf("  %s · %s · %s · %s", run.HeadBranch, run.Event, run.Actor.Login, elapsed(run, time.Now()))) + "\n")

	if m.loading {
		b.WriteString(dimStyle.Render("loading…") + "\n")
	}
	if m.jobErr != nil {
		b.WriteString(errStyle.Render(clip(m.jobErr.Error(), m.width)) + "\n")
	}
	rows := m.bodyRows() - 2
	start := 0
	if m.dcursor >= rows {
		start = m.dcursor - rows + 1
	}
	for i := start; i < len(m.rows) && i < start+rows; i++ {
		line := m.rowText(m.rows[i])
		if i == m.dcursor {
			line = selStyle.Render(clip(stripANSI(line), m.width-2))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(m.footer("j/k move · enter logs · e first failed step · R rerun · esc back"))
	return b.String()
}

func (m Model) rowText(r detailRow) string {
	j := m.jobs[r.job]
	if r.step < 0 {
		ic, label, st := badge(j.Status, j.Conclusion)
		return st.Render(fmt.Sprintf("%s %s", ic, label)) + " " + j.Name + dimStyle.Render("  "+dur(j.CompletedAt.Sub(j.StartedAt)))
	}
	s := j.Steps[r.step]
	ic, _, st := badge(s.Status, s.Conclusion)
	return "    " + st.Render(ic) + " " + dimStyle.Render(fmt.Sprintf("%2d", s.Number)) + " " + s.Name
}

func (m Model) viewLog() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(clip("log · "+m.logTitle, m.width)) + "\n")
	if m.loading {
		b.WriteString(dimStyle.Render("loading…") + "\n")
	}
	if m.logErr != nil {
		b.WriteString(errStyle.Render(clip(m.logErr.Error(), m.width)) + "\n")
	}
	rows := m.bodyRows()
	for i := m.logOff; i < len(m.logLines) && i < m.logOff+rows; i++ {
		line := clip(m.logLines[i], m.width)
		if strings.Contains(line, "##[error]") {
			line = errStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	pct := 100
	if n := len(m.logLines); n > 0 {
		pct = min(100, (m.logOff+rows)*100/n)
	}
	b.WriteString(m.footer(fmt.Sprintf("j/k scroll · pgup/pgdn page · g/G top/end · esc back · %d%%", pct)))
	return b.String()
}

// stripANSI removes SGR sequences so a selected row can be re-styled whole.
func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			in = true
		case in && r == 'm':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}
