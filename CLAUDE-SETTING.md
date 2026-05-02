# CLAUDE-SETTING.md

How ahjo wires Claude Code into containers, and why this exact approach.

## TL;DR

Each ahjo container runs Claude Code with two pieces of state:

1. `CLAUDE_CODE_OAUTH_TOKEN` — a 1-year static bearer minted by `claude setup-token`, forwarded into every container via COI's `forward_env`.
2. `hasCompletedOnboarding: true` — written into the **host VM's** `~/.claude.json` once. COI copies that file into each container at startup, so containers inherit the marker.

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

COI **copies the host's `~/.claude.json` into every container at startup**, overwriting whatever was baked into the `ahjo-base` image. So:

- Putting the marker in `ahjo-base/build.sh` → no-op (overwritten).
- Putting the marker in the host VM's `~/.claude.json` → propagates into every container via COI's copy.

`ahjo init` merges `hasCompletedOnboarding: true` and `lastOnboardingVersion` into the host VM's `~/.claude.json` once. See `internal/cli/init.go` (`mergeClaudeOnboardingMarker`).

## Per-container defaults (model, effort, prompt suppressors)

COI's claude integration writes `~/.claude/settings.json` and `~/.claude.json` during session setup. Among other things, it injects:

- `effortLevel: "<level>"` (configured-tier setting)
- `env: { CLAUDE_CODE_EFFORT_LEVEL: "<level>" }` (env-var override block)

driven by `[tool.claude] effort_level` in the per-worktree `.coi/config.toml`, defaulting to `"medium"` when unset. Both fields are written to the same value. The env-var block is the problem: env-var values have the **highest precedence** in claude's effort resolution, so any value persisted there locks the user's `/effort` slider — every change shows "CLAUDE_CODE_EFFORT_LEVEL=… overrides this session". This is true regardless of which level COI was configured to write.

**`ahjo-claude-prepare`** (baked into the `ahjo-base` image, run once per container by `ahjo shell` immediately after COI's first session-setup, before claude ever launches) repairs this. In a single pass it:

1. Strips the `CLAUDE_CODE_EFFORT_LEVEL` key out of the `env` blocks in both `~/.claude/settings.json` and `~/.claude.json` (and removes the surrounding `env` object if that was its only key — but preserves any other env vars the user may have).
2. Plants ahjo's defaults that the user *can* change later: `model: "opusplan"`, `effortLevel: "high"`, plus `skipDangerousModePermissionPrompt: true` and `projects["/workspace"].hasTrustDialogAccepted: true` to silence the two first-run prompts.

After this runs, `/model` and `/effort` work cleanly from the TUI — no env-var override warning, the user can pick any level. Because ahjo containers are persistent, COI's session-setup pipeline only fires on first creation, so a one-shot strip via the `$HOME/.ahjo-claude-prepared` marker is sufficient — there's no resume path that would re-inject the env block.

The reason ahjo doesn't try to set `[tool.claude] effort_level` in the COI config is that doing so still leaves the env block in place — COI writes it unconditionally. The only way to give the user a usable `/effort` slider is to delete the block after COI writes it.

## End-to-end

```
ahjo init  (interactive, on Mac's Lima VM or Linux host):
  step:  claude setup-token              → token saved in ~/.ahjo/.env
  step:  merge {hasCompletedOnboarding}  → host VM's ~/.claude.json

ahjo new <repo> <branch>:
  → writes .coi/config.toml with forward_env = ["CLAUDE_CODE_OAUTH_TOKEN"]

ahjo shell <alias>:
  → tokenstore.Load() exports the token from ~/.ahjo/.env into the VM shell
  → coi shell forwards exported env var into the container shell
  → COI copies host's ~/.claude.json into /home/code/.claude.json
  → user runs `claude` → drops to prompt, "Claude API" header, no friction
```

## When something breaks

- **Container prompts for theme/login on first run** → host VM's `~/.claude.json` is missing `hasCompletedOnboarding`. Re-run `ahjo init`; the marker step is idempotent.
- **TUI says "Not logged in", header reads "API Usage Billing"** → env var isn't reaching `claude`'s process. From inside the container, `export -p | grep CLAUDE_CODE_OAUTH` should print one line; if it doesn't, exit and re-enter via `ahjo shell` so COI re-runs `forward_env`.
- **Future Claude release introduces a new onboarding gate** → bump `claudeOnboardingVersion` in `internal/cli/init.go` to the new version.
