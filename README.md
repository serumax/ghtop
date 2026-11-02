# ghtop

A k9s/htop-style live view of GitHub Actions runs across every organization
and repository you can reach through `gh`. It answers one question: what is
deploying right now, and what just broke.

    ghtop                          # every org you belong to + your own repos
    ghtop -orgs ida,ida-cclm       # restrict to some orgs
    ghtop -interval 60s -max-repos 25

Requires `gh` authenticated (`gh auth status`). ghtop never reads tokens itself.

## How it polls

GitHub has no organization-wide runs endpoint, so ghtop sweeps repos:

- Repo list: all orgs, reloaded every `-repo-refresh` (15 min).
- Watch set: repos pushed in the last 72 h, newest first, capped at `-max-repos`.
- Sweep: every `-interval` (30 s), the last 5 runs of each watched repo.
  The interval quadruples when the REST quota drops below 500.

The header shows the remaining REST quota. Each sweep costs one request per
watched repo, so keep `-max-repos` near the number of repos that actually deploy.

## Keys

| Screen | Key | Action |
| --- | --- | --- |
| list | `j`/`k`, `g`/`G` | move |
| list | `enter` / `l` | open run detail (jobs and steps) |
| list | `f` | cycle filter: all, active, failed |
| list | `/` | search repo, workflow, branch, event, actor, title; `esc` clears |
| list | `o` | open run in browser |
| list | `R` then `y` | re-run a completed run (asks first) |
| detail | `j`/`k` | move across jobs and steps |
| detail | `enter` / `l` | open the job log |
| detail | `e` | jump to the first failed step |
| log | `j`/`k`, `pgup`/`pgdn`, `g`/`G` | scroll; `##[error]` lines are red |
| any | `esc` / `h` | back |
| any | `q` | quit |

## Limits

- Logs come from GitHub's job-log endpoint. It serves finished jobs reliably;
  logs of jobs still running may be partial or unavailable, and the error is shown.
- Responses are not cached with ETags yet, so every sweep spends quota.

## Build

    go build -o ghtop . && install ghtop ~/.local/bin/
