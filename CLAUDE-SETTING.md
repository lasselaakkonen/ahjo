# CLAUDE-SETTING.md

How ahjo wires Claude Code into containers, and why this exact approach.

## TL;DR

Each ahjo container runs Claude Code with two pieces of state:

1. `CLAUDE_CODE_OAUTH_TOKEN` — a 1-year static bearer minted by `claude setup-token`, forwarded into every container via the `forward_env` knob (set per repo, applied with `incus exec --env`).
2. `hasCompletedOnboarding: true` — written into the **host VM's** `~/.claude.json` once. `ahjo repo add` pushes that file into each container's `/home/ubuntu/.claude.json`, so containers inherit the marker.

Both are set by `ahjo init`. No `claude /login`. No per-container JSON writes. No `oauthAccount` propagation.

## Auth precedence

Claude reads credentials in this order ([docs](https://code.claude.com/docs/en/authentication#authentication-precedence)):

1. Cloud provider (Bedrock / Vertex / Foundry)
2. `ANTHROPIC_AUTH_TOKEN`
3. `ANTHROPIC_API_KEY`
4. `apiKeyHelper` script
5. **`CLAUDE_CODE_OAUTH_TOKEN`** ← ahjo's path
6. Subscription OAuth from `~/.claude/.credentials.json` (written by `/login`)

The TUI header shows which path is active:

- **"Claude API"** → rank 5, env-var auth working. This is what ahjo expects.
- **"API Usage Billing"** → rank 3 fallback when no real credential is in scope. Means *broken* — usually the env var got lost (e.g. shell `unset` + non-`export` re-set).
- Rank-6 subscription auth shows a different label tied to your plan; ahjo never goes here.

## `setup-token` vs `/login`

Both walk the same browser-OAuth flow on the host. They persist different things:

| | `setup-token` | `/login` |
|---|---|---|
| Output | prints a 1-year static bearer | writes `~/.claude/.credentials.json` (refresh-token, single-use) + `oauthAccount` block in `~/.claude.json` |
| Refresh dance | none | yes — single-use refresh token swapped each refresh |
| Race across N concurrent containers | safe | broken — first refresh wins, others re-auth |
| Picker suppressed | yes (env-var auth bypasses the subscription branch entirely) | yes (via `oauthAccount`) |
| `hasCompletedOnboarding` written | no | no |

ahjo uses `setup-token` because the static token has no refresh race, and the picker problem is solved by env-var auth winning precedence — Claude never asks "which account?" when rank 5 is active.

## The actual cause of "fresh container prompts the user"

It is *not* the account picker. It is Claude's **first-run onboarding** flow — theme → ToS → login-method. That flow is gated only on:

```json
{ "hasCompletedOnboarding": true, "lastOnboardingVersion": "<version>" }
```

in `~/.claude.json`. `setup-token` does not write this. `/login` writes it as a side effect of completing onboarding.

## Why the marker lives on the host VM, not in the image

`ahjo repo add` **copies the host VM's `~/.claude.json` into each container at creation** (see `pushClaudeConfig` in `internal/cli/repo.go`), overwriting whatever was baked into the `ahjo-base` image. So:

- Putting the marker in the `ahjo-runtime` Feature's `install.sh` → no-op (overwritten at repo-add time).
- Putting the marker in the host VM's `~/.claude.json` → propagates into every container via the per-repo file push.

`ahjo init` merges `hasCompletedOnboarding: true` and `lastOnboardingVersion` into the host VM's `~/.claude.json` once. See `internal/cli/init.go` (`mergeClaudeOnboardingMarker`).

## Per-container defaults (model, effort, prompt suppressors)

**`ahjo-claude-prepare`** is baked into `ahjo-base` by the `ahjo-runtime` devcontainer Feature (see `internal/ahjoruntime/feature/install.sh`) and run once per container by `ahjo repo add` immediately before claude ever launches. It plants ahjo's defaults that the user *can* change later: `model: "opusplan"`, `effortLevel: "high"`, plus `skipDangerousModePermissionPrompt: true` and `projects["/repo"].hasTrustDialogAccepted: true` to silence the two first-run prompts. All settings.json fields, so `/model` and `/effort` overwrite cleanly from the TUI.

The script is idempotent via `$HOME/.ahjo-claude-prepared`. It reads `HOME` from `getent` so it works even under `incus exec --user 1000` with a sparse environment, and never names a user — every path is derived from `$HOME`, so a future user rename touches only the build pipeline.

## End-to-end

```
ahjo init  (interactive, on Mac's Lima VM or Linux host):
  step:  claude setup-token              → token saved in ~/.ahjo/.env
  step:  merge {hasCompletedOnboarding}  → host VM's ~/.claude.json

ahjo repo add <repo>:
  → incus init ahjo-base, then push host's ~/.claude.json into
    /home/ubuntu/.claude.json (and ~/.claude/* into /home/ubuntu/.claude/)
  → run ahjo-claude-prepare once to plant model/effort defaults
  → forward_env from ~/.ahjo/config.toml is applied via incus exec --env
    on every shell/claude invocation

ahjo shell <alias>  (or ahjo claude <alias>):
  → tokenstore.Load() exports the token from ~/.ahjo/.env into the VM shell
  → incus exec --force-interactive forwards CLAUDE_CODE_OAUTH_TOKEN via
    --env, then drops to bash (or directly to `claude`) as the in-container
    `ubuntu` user — prompt, "Claude API" header, no friction
```

## When something breaks

- **Container prompts for theme/login on first run** → host VM's `~/.claude.json` is missing `hasCompletedOnboarding`. Re-run `ahjo init`; the marker step is idempotent.
- **TUI says "Not logged in", header reads "API Usage Billing"** → env var isn't reaching `claude`'s process. From inside the container, `export -p | grep CLAUDE_CODE_OAUTH` should print one line; if it doesn't, exit and re-enter via `ahjo shell` so the `--env` forwarding runs again.
- **Future Claude release introduces a new onboarding gate** → bump `claudeOnboardingVersion` in `internal/cli/init.go` to the new version.
