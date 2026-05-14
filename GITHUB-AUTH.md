# GitHub auth inside containers

Each `ahjo repo add` for a GitHub remote prompts for a fine-grained personal access token scoped to that one repo. The token is forwarded into the matching container as `GH_TOKEN`. Inside the container, both `gh` (API ops) and `git` (clone/fetch/push, via the credential helper) authenticate using that single token. One compromised container can read/write **only** the repo its token is scoped to.

## TL;DR

| Today | Before |
| --- | --- |
| Per-repo fine-grained PAT, forwarded as `GH_TOKEN` | Host's SSH agent forwarded into every container |
| `gh pr create / merge / api …` works in-container | `gh` had no auth — failed silently |
| `git clone/push` over HTTPS with the credential helper | `git` over SSH with the host's agent (broad blast radius) |
| Token scope is enforced by GitHub: one repo, one PAT | Whatever was in `ssh-add -l` had write to every repo it could touch |
| `ahjo repo rm` deletes the local token file | n/a |

No `gh` host-side dependency on Mac or Linux. ahjo never talks to the GitHub API directly. The only thing on the host is `gh auth login`'s state, which ahjo does **not** touch.

## When does this trigger?

Only for GitHub remotes. `ahjo repo add` detects `git@github.com:o/r.git`, `https://github.com/o/r`, etc. and prompts. Non-GitHub remotes (GitLab, custom git servers, local paths) bypass the prompt — auth there is whatever git already has (SSH agent forwarding still works for those).

## Required setup per repo

```
ahjo repo add lasselaakkonen/some-repo
```

ahjo prints:

```
Create a fine-grained GitHub PAT for lasselaakkonen/some-repo:

  https://github.com/settings/personal-access-tokens/new

  Repository access:  Only select repositories → lasselaakkonen/some-repo
  Permissions:        Contents              read/write
                      Pull requests         read/write
                      Issues                read/write
                      Metadata              read (granted automatically)
  Expiration:         your call (max 1 year)

Paste the token (begins with github_pat_…):
>
```

Open the link, configure as shown, copy the token, paste into the prompt. Done. ahjo writes it to `~/.ahjo/repo-tokens/<slug>.env`, sets `GH_TOKEN` on the container, runs `gh auth setup-git` so git's credential helper uses the token, and proceeds with the clone.

For non-interactive setups, use `--token-file`:

```
ahjo repo add lasselaakkonen/some-repo --token-file ~/.secrets/some-repo.env
```

The file can be a raw token on a single line, or an env-file with `GH_TOKEN=…` / `GITHUB_TOKEN=…`.

## Storage

```
~/.ahjo/repo-tokens/<slug>.env       # mode 0600
```

Contents: one line, `GH_TOKEN=github_pat_…`. The file is local-only — never copied into containers verbatim, never pushed anywhere. The token value travels through `incus config set environment.GH_TOKEN …` so the container sees it as an env var.

Branch containers (created via `ahjo create`) inherit the same `GH_TOKEN` via `incus copy`, which propagates `environment.*` keys. So every branch container of the same repo shares one token, just like they share git config and ssh config.

## In-container behavior

Inside any container `ahjo` built for repo X:

```bash
gh auth status     # logged in to github.com (GH_TOKEN)
gh pr list         # works — lists PRs on repo X
gh pr create ...   # works — creates PR on repo X
gh pr view 42      # works
gh api repos/X/keys              # works
gh api repos/Y/keys              # 403 — PAT not scoped to Y

git push origin foo     # works — credential helper returns the PAT
git clone https://github.com/Y/...   # 403 — same reason
```

The PAT's "Only select repositories" setting is a hard fence. The container can read/write exactly the repos in the token's scope, nothing else — including no other repos the user owns, public or private. (Public repos owned by *other* users remain anonymously readable, as they would be without any auth.)

## How `git` uses the token

`gh auth setup-git` (run once during `ahjo repo add`) writes to `~/.gitconfig`:

```
[credential "https://github.com"]
    helper = !gh auth git-credential
[credential "https://gist.github.com"]
    helper = !gh auth git-credential
```

Every git operation against `https://github.com/…` then calls `gh auth git-credential get`, which reads `GH_TOKEN` from env and returns it as the credential. Transparent to the user.

The clone URL is rewritten to HTTPS during `repo add` even if the input was SSH — `git@github.com:o/r.git` becomes `https://github.com/o/r.git` so the credential helper actually gets used. If we left it SSH, git would silently fall back to the host's forwarded ssh-agent and use a credential that isn't scoped to this repo.

## Removal

```
ahjo repo rm <alias>
```

Tears down the container (existing behavior), removes `~/.ahjo/repo-tokens/<slug>.env`, and prints:

```
note: revoke the PAT for <slug> in GitHub if you no longer need it:
  https://github.com/settings/personal-access-tokens
```

ahjo cannot revoke a PAT — GitHub's API doesn't expose PAT deletion. Manual revocation is on the user. If you forget, the token just expires (max 1 year) on its own.

## Token rotation

To rotate a PAT (compromise suspected, scope change, expiration approaching):

1. Create a new PAT in the GitHub UI with the desired scope/expiration.
2. Overwrite the file: `printf 'GH_TOKEN=%s\n' "$NEW_TOKEN" > ~/.ahjo/repo-tokens/<slug>.env`
3. `incus config set ahjo-<slug> environment.GH_TOKEN "$NEW_TOKEN"`
4. Repeat step 3 for every branch container: `for c in $(incus list ahjo-<slug>-* -c n --format csv); do incus config set "$c" environment.GH_TOKEN "$NEW_TOKEN"; done`
5. Revoke the old PAT in the GitHub UI.

Step 4 is required because `incus copy` snapshotted the env at branch-create time. (A future `ahjo repo rotate-token` subcommand could automate this.)

## What about non-GitHub remotes?

`ahjo repo add gitlab.com/foo/bar` (or any URL that doesn't match `github.com`) skips the prompt entirely. Auth is whatever the host's git already provides — typically the forwarded ssh-agent (the pre-existing model). If you want per-repo scoping for non-GitHub, you're on your own; each forge has its own token model.

## Why fine-grained PATs and not classic PATs

Classic PATs have whole-account scopes (`repo` = all your repos, public + private). A leak from one container would give an attacker write access to every repo you own. Fine-grained PATs with "Only select repositories" enforce per-repo isolation at the GitHub side — the token literally can't reach other repos, regardless of what code runs inside the container.

## Why not deploy keys

We considered (and prototyped) registering a per-repo deploy key via `gh api` during `repo add`. The deploy key would cover git transport over SSH, scoped to one repo, automatically created and revoked by ahjo. It works.

The fine-grained PAT does everything the deploy key does *plus* the GitHub API surface (`gh pr create`, `gh issue create`, …) — which is what users actually need inside an ahjo container, since the whole point of ahjo is running Claude Code with full PR workflows. Deploy keys cover the easy half. The PAT covers all of it.

The trade-off: PATs must be created in the GitHub web UI; ahjo can't mint them. That's one extra click per repo. For an isolation gain you can't get from a deploy key (per-repo API scoping), the click is worth it.
