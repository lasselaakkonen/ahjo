# Lima ssh ControlMaster freezes PAM-time state

`~/.lima/<vm>/ssh.config` uses `ControlMaster auto` + `ControlPersist yes`. The first ssh into the VM opens a master at `~/.lima/<vm>/ssh.sock`; every subsequent `limactl shell` multiplexes through it. PAM evaluates supplementary groups + `SSH_AUTH_SOCK` *once*, at master-creation time — host-side changes to either don't propagate to multiplexed sessions until the master is reset.

**When this bites:** any host-side change that PAM/login normally evaluates — `usermod -aG <group>`, persisting a new agent socket, `/etc/security/*` tweaks. Sessions look identical (`whoami`, `id` runs fine) but quietly miss the new state. e.g. `usermod -aG incus-admin lasse` during init leaves `limactl shell ahjo bash -c id` showing `groups=1000(lasse)` only — every `incus` call from that session fails with "permissions denied".

**Reset:** `lima.CloseSSHControlMaster(vmName)` — see `internal/lima/env_darwin.go:104`. Already wired into `saveAgentChoice` (`cmd/ahjo/agent_step_darwin.go:73`) and into the in-VM bring-up + update steps in `cmd/ahjo/main_darwin.go`. Manual one-shot: `ssh -F ~/.lima/<vm>/ssh.config -O exit lima-<vm>`.

**Diagnostic:** `limactl shell <vm> bash -c id` vs a fresh `ssh -p <port> <user>@127.0.0.1 -i ~/.lima/_config/user id`. Different supplementary-group sets ⇒ stale master. Don't reach for `sg`-wrapping incus/coi calls as a "fix" — those failures are downstream of master state.
