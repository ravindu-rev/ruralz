# Benchmark hosts runbook

This directory sets up the hosts of the benchmark harness, `test/bench/harness`, and of `chaos-scale.yml`, as [Reference hardware](../../docs/architecture/12-performance-budgets-and-benchmarking.md#reference-hardware) defines them:

| File | Contents |
|---|---|
| `rh-1.json`, `rh-3.json`, `loopback.json` | One profile inventory per profile, which `harness.ReadProfile` reads strictly: each host's address, ssh destination, cpusets and working directory, the dedicated user and its key, the fixed secret root and TLS directory, the services' fixed addresses that the profile's overlays name, and whether scopes and host checks apply |
| `provision.sh` | Run from a workstation: provisions the four hosts of an inventory over ssh with `host.sh`, then checks that RH-1 reaches the others as the harness does |
| `host.sh` | Run as root on one host: applies the settings baseline for the host's role |
| `tune.sh` | The part of the baseline that a reboot undoes, which `ruralz-bench-tune.service` applies at every boot |
| `runner.sh` | The `rh-1` runner on RH-1: `runner.sh install`, and `runner.sh run` for `ruralz-bench-runner.service` |

`test/bench/harness` checks the inventories against the overlays and the settings baseline, and `test/bench/benchhost` runs the scripts against a fake host. Changing a value of the settings baseline, an inventory included, is a `perf` pull request.

## Hosts

| Host | Role in the inventory | Runs |
|---|---|---|
| RH-1 | `gateway` | `ruralzd` in its 4-core cpuset, the `rh-1` runner and the harness on the other cores |
| RH-1L | `load` | `internal/testkit/loadgen`, oha and vegeta through `test/bench/labhost` |
| RH-1U | `upstream` | The mock Upstream, the mock IdP and the OTLP sink |
| RH-1S | `statestore` | Valkey 9 from the pinned image, and S5x's fault proxy |

Rent four bare-metal machines of the RH-1 specification on one switch, with a minimal Debian stable install, root ssh access (or a user with passwordless `sudo`) and an ed25519 ssh host key. Every host runs nothing else.

The inventories name the hosts by their `bench.example` names, which `host.sh` writes into `/etc/hosts` on every host with the lab addresses given to `provision.sh`; the harness reaches RH-1L, RH-1U and RH-1S over ssh by those names on the lab network.

## Before provisioning: the RH-1 inventory

`rh-1.json` follows the settings baseline on a 16-core host whose cores share the NIC's NUMA node: cores 0 and 1 take the interrupts, `ruralzd` 2-5, the load tools 2-9, the mock 2-9 with the sink and IdP on 10-13, Valkey 2-5 with the fault proxy on 6-7. When a rented host differs, change its cpusets in a `perf` pull request before provisioning: each measured cpuset must lie on the NIC's NUMA node with at least one core of that node left over for the NIC's interrupts, which `tune.sh` checks and fails on.

## Provisioning

From a checkout, on a workstation with bash 4.4 or later, ssh, scp and jq:

```bash
bash deploy/benchhost/provision.sh --inventory deploy/benchhost/rh-1.json \
  --admin gateway=root@RH1 --admin load=root@RH1L --admin upstream=root@RH1U --admin statestore=root@RH1S \
  --lab gateway=10.0.0.1 --lab load=10.0.0.2 --lab upstream=10.0.0.3 --lab statestore=10.0.0.4
```

`--admin` takes each host's ssh destination and `--lab` its address on the lab network. A re-run applies the same baseline and changes nothing. On every host `host.sh`:

1. Checks the host runs Debian, installs what the role needs (`docker.io` on RH-1S; git, make, curl, openssh-client and openssl on RH-1), removes `unattended-upgrades` and masks the apt timers.
2. Creates the inventory's `sshUser`, `ruralz-bench`, with lingering and a user manager to which cpu, cpuset, io, memory and pids are delegated, so the harness's `systemd-run --user --scope -p AllowedCPUs=` scopes hold their cpusets; it sets `LimitNOFILE` 1,048,576 for its sessions and manager.
3. Creates the working directory and gives it to `ruralz-bench`, who makes the secret root and the TLS directory inside it.
4. Writes the `bench.example` names into `/etc/hosts`.
5. On RH-1, creates the harness's ed25519 key pair at `sshKey` and writes `knownHosts` from the other hosts' host keys; elsewhere it authorizes that key for `ruralz-bench`, only from RH-1's lab address. It does both as `ruralz-bench`: under a directory the user owns, its home and the working directory, a link the user planted would lead a change made as root elsewhere.
6. Writes the sysctls of the baseline (`tcp_tw_reuse` on RH-1 and RH-1L only), turns swap off and masks `irqbalance`.
7. Installs `tune.sh` and `ruralz-bench-tune.service`, and runs it: SMT off, turbo off, the `performance` governor with the minimum frequency at the fixed maximum of 3.0 GHz or more, idle states deeper than C1 disabled, transparent huge pages `madvise`, the lab NIC's rings at their maximum with adaptive coalescing off and `rx-usecs` and `tx-usecs` 0, and its interrupts on the cores of its NUMA node outside the host's cpusets. On `rh-1`, a setting the host cannot apply fails the run.
8. On RH-1S, pulls the Valkey image `redisserver.ImageValkey9` pins and adds `ruralz-bench` to the `docker` group: the harness runs that image for each run as `ruralz-bench`, on Valkey's cpuset.
9. Holds every installed package.

## The rh-1 runner

The runner on RH-1 is an ephemeral just-in-time runner: `ruralz-bench-runner.service`, as root, holds a GitHub App's private key, registers one runner labelled `self-hosted` and `rh-1`, copies root's `actions/runner` install into a fresh directory under `/var/lib/ruralz-bench-runner`, runs the copy as `ruralz-bench` until it has taken one job, deletes it and registers the next. It runs on the cores outside `ruralzd`'s cpuset with `LimitNOFILE` 1,048,576. The App's JWT, its installation tokens and the runner's configuration never appear on a command line.

Each runner's job-started hook, `runner.sh admit`, runs before any step of the job the runner took and lets it run only when it is a job of `nightly.yml`, `chaos-scale.yml` or `release.yml` of this repository on a protected branch or tag, scheduled, dispatched or pushed. It refuses any other job, a pull request's above all, in the journal (`journalctl -t ruralz-bench-runner`) and stops it by killing the runner; GitHub then fails the job.

1. Create a GitHub App owned by the repository owner, without a webhook, with the repository permission Administration: read and write (Metadata: read comes with it), and install it on this repository only. If the repository moves to an organization, give the App the organization permission Self-hosted runners: read and write instead, and create a runner group limited to this repository.
2. Generate the App's private key, copy it to RH-1 and delete every other copy: the key never leaves RH-1.
3. Take the newest `actions/runner` release and the SHA-256 of its `actions-runner-linux-x64-<version>.tar.gz` from its release notes.
4. On RH-1, as root, from a checkout of the repository:

   ```bash
   bash deploy/benchhost/runner.sh install --inventory deploy/benchhost/rh-1.json \
     --app-id APP_ID --app-key /root/app.pem --repo ravindu-rev/ruralz \
     --runner-version VERSION --runner-sha256 SHA256
   ```

   Add `--runner-group ID` for the organization runner group. Then remove `/root/app.pem`: the service reads its copy in `/etc/ruralz-bench`.
5. Check that the repository's runners list an online runner labelled `rh-1` (Settings, Actions, Runners).

## Turning RH-1 on

1. The repository secret `REPO_SETTINGS_TOKEN`, the fine-grained read-only token with Administration: read that `repo-settings.yml` uses, also lets nightly's RH-1 runner status job list the runners.
2. Set the repository variable `RH1_PROVISIONED` to `true`. Nightly's Latency job, the release soak job and `chaos-scale.yml` then run on `rh-1`, one at a time in the `rh-1` concurrency group, and the runner status job opens or updates the "RH-1 runner offline" `nightly-failure` issue whenever no online runner carries the label.
3. Run `nightly.yml` on demand from `main`: its Latency job checks every host before each scenario and fails with an "RH-1 host check failed" annotation when a host's frequency, throttle counters or round trip are off.
4. `chaos-scale.yml` reads the same inventory; its `target` profile stops before its first run until the chaos harness places the State Store, mocks and load on RH-1S, RH-1U and RH-1L, RH-1L returning every request's record once the load ends ([Scale profiles](../../docs/engineering/03-testing-and-quality-strategy.md#scale-profiles)).

Only protected branches and tags run on `rh-1`: the runner's hook admits no job of another ref or of a pull-request event, and each `rh-1` job's first step refuses them too, so the rulesets `scripts/repo-settings.sh` applies to `main`, `release-*` and `v*` tags must be in place first.

## Maintenance window

Updates happen only in a recorded maintenance window, so a night's A/B runs share one kernel. On each host: `apt-mark unhold` the packages to update, update them, reboot, then re-run `provision.sh`, which holds every package again and re-applies the baseline. Each benchmark record names the kernel and settings it ran with.

## RH-3 and loopback

`rh-3.json` is the reproduction profile: a cloud VM whose 4 dedicated vCPUs take the Node, plus helper VMs for the load tools, the mock and sink, and the State Store. It is never gated, so it runs no host checks, and `tune.sh` skips with a line each setting a VM does not expose. Its cpusets are RH-1's, so each VM needs the cores they name (the gateway VM 2-5 and at least one more for the harness); adapt them to the VMs' shapes first, as for RH-1. Provision it the same way with `--inventory deploy/benchhost/rh-3.json`; it has no runner. On its gateway VM, as `ruralz-bench`, from a checkout, `make bench-latency BENCH_PROFILE=$PWD/deploy/benchhost/rh-3.json BENCH_ROTATION_ALL=true` runs every scenario.

`loopback.json` is the loopback profile at fixed directories: every service on one Linux host, without scopes or host checks, and with an empty `stateStoreServer`, the pinned Valkey image through Docker. Without `BENCH_PROFILE` the harness builds the same profile in a temporary directory and runs the `valkey-server` or `redis-server` it finds.
