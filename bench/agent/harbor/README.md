# Harbor tasks

The agent benchmark's tasks in the [Harbor](https://github.com/harbor-framework/harbor)
format, one task directory per experiment arm:

- **Track A (incidents):** `<task>-control` and `<task>-kavach`, checked with
  `kavach diff` (`redis-10068`, `redis-10764`, `redis-10858`, `redis-11734`,
  `redis-12082`).
- **Track B (features):** four arms per task (`go-genai-streamed-function-args`,
  `httpx-deterministic-cookie-store`, `kombu-single-active-consumer-priority`,
  `kombu-virtual-queue-dead-lettering`, `updo-policy-alerting`), checked with
  `kavach contract check`:
  - `<task>-control`: base commit and the feature specification;
  - `<task>-workflow`: base commit and the contract-first workflow using the
    project's own focused test suite;
  - `<task>-kavach`: base commit plus the Kavach integration, a locked
    behaviour contract (`/task/contract`), the `kavach` CLI and its MCP server;
  - `<task>-kavach-implement`: base commit plus the Kavach integration and
    `kavach` tools, with no contract supplied (the agent records traffic,
    declares/checks invariants, and initializes and locks the contract itself).

Harbor replaces `../harness/` for running agents.

## Layout

```
tasks/redis-10068-control/      Redis at the base commit, the bug report
tasks/redis-10068-kavach/       the same plus the Kavach integration, the
                                fixture, the kavach CLI and its MCP server
tasks/<track-b>-control/        base commit, the feature specification
tasks/<track-b>-workflow/       base commit, the contract-first test workflow
tasks/<track-b>-kavach/         base commit + Kavach integration, locked
                                /task/contract, kavach CLI and MCP server
tasks/<track-b>-kavach-implement/
                                base commit + Kavach integration and kavach
                                tools; agent builds and locks contract/
  task.toml
  instruction.md                ../tasks/<task>/task.md, with the arm's section
  environment/docker-compose.yaml   the agent's image
  tests/docker-compose.yaml     the verifier's image
  tests/test.sh                 runs the task's grade.sh, writes the reward
                                and kavach.json
  solution/                     applies the reference solution (oracle only)
check-candidates.sh             grades every candidate fix of redis-10068
                                through Harbor
```

## Images

The images are targets of each task's `../tasks/<task>/Dockerfile` (`control`,
`workflow`, `kavach` or `kavach-implement` for the agent; `grader` for the
verifier), built from the repository root because the image builds Kavach from
source. The compose files do not build them: a remote provider (Daytona,
Modal, Vercel) uploads only `environment/` and `tests/`, so there is no
repository to build from. Each `task.toml` names a prebuilt image,
`[environment] docker_image` for the agent and
`[verifier.environment] docker_image` for the verifier, and Harbor then
composes `docker-compose-prebuilt.yaml` (`image: ${PREBUILT_IMAGE_NAME}`)
under the task's compose file instead of its build file
(`environments/definition.py` 26-36, `docker/docker.py` 391-409, 1035-1047;
Daytona `environment.py` 538-545, 756-770, Modal `modal.py` 282, Vercel
`vercel/compose.py` 411-417, 481). The verifier's `tests/` has a compose file
and no Dockerfile, so the same rule applies there
(`models/task/verifier_mode.py` 88-107). A remote provider pulls the image
into its sandbox's Docker; its `docker compose build` finds nothing to build.

Images are named `ghcr.io/kavachlabs/bench-<task>:<target>-<key>`. `<key>`
is a hash of the task's Dockerfile, of every repository file its `COPY`
lines name (path, executable bit and content, through `git hash-object`) and
of the build context's `.dockerignore` files (`image-tag.py`; it needs no
build and no network). `sync-tasks.py` writes the references
into the `task.toml` files and `sync-tasks.py --check` fails when one is stale,
which is what happens after any change to a Dockerfile or to the Go, SDK or
task files it copies. The workflow `.github/workflows/bench-images.yml`
builds and pushes the missing ones on every push to main (linux/amd64 and
linux/arm64, merged into one manifest list; kombu-virtual-queue-dead-lettering
is amd64 only because its base image is).

Locally, nothing is pulled while the image exists:

```sh
bench/agent/harbor/image-tag.py build redis-10068            # every target
bench/agent/harbor/image-tag.py build redis-10068 control    # one
bench/agent/harbor/sync-tasks.py                              # after changing a Dockerfile
```

`build` refuses when a file the Dockerfile copies is untracked or modified
(the image would carry the key CI computes from the committed files);
`--allow-dirty` overrides it.

`build` tags the image with exactly the name the `task.toml` carries, so
Compose uses it and Harbor does not need `--force-build` (which would not build
anyway: there is no Dockerfile in `environment/`). A reference that is neither
built nor pushed fails with `pull access denied`. To push by hand, log in with
a token that has `write:packages` (`gh auth token | docker login ghcr.io -u
<user> --password-stdin`) and `docker push` the ref; for both architectures use
`docker buildx build --platform linux/amd64,linux/arm64 --push` with the same
arguments as `image-tag.py build`.

The workflow's build cache is a registry cache, `ghcr.io/kavachlabs/bench-cache:<task>-<arch>`
(`type=registry,mode=max`; the repository's 10 GB Actions cache is too small
for 11 tasks on two architectures). Only CI reads it, so `bench-cache` can stay
private. A task whose architecture build fails is skipped at the merge step,
and the merge job fails to show it; the other tasks still merge.

### Making the packages public

Sandboxes pull anonymously: Harbor has no registry credentials for Daytona,
Modal or Vercel, so every `bench-*` package must be public. `GITHUB_TOKEN`
cannot change a package's visibility, and a new container package starts
private, so after the first push of each one, once:

1. github.com/orgs/kavachlabs/packages, open `bench-<task>`.
2. Package settings, Danger Zone, Change visibility, Public, confirm by typing
   the name.
3. Check from a machine with no login: `docker logout ghcr.io; docker pull
   ghcr.io/kavachlabs/bench-<task>:<target>-<key>`.

Visibility belongs to the package, not the tag, so later pushes need nothing.
The images carry the label `org.opencontainers.image.source`, which links the
package to this repository (access and the package page, and write access for
the workflow's `GITHUB_TOKEN`); GitHub documents that a linked package
inherits the repository's *permissions*, but not that it becomes public, so
plan on the manual step above. The images hold the hidden tests of the
`grader` target: public packages publish them. That is already true of this
repository's `hidden/` directories if it is public.

## Running

Needs Docker with Compose (Colima works) and `harbor` (0.24.0), and the task's images ("Images"). Colima shares
only `$HOME` with its VM and Harbor mounts each trial's directory into the
containers, so the jobs directory must be under `$HOME`. Give it as an
absolute path: with a relative one Harbor's first upload to the verifier fails
and it falls back to a slower copy.

```sh
jobs=$PWD/bench/agent/results/harbor     # gitignored
harbor run -p bench/agent/harbor/tasks -a oracle -o "$jobs" -y   # reward 1
harbor run -p bench/agent/harbor/tasks -a nop    -o "$jobs" -y   # reward 0
harbor run -p bench/agent/harbor/tasks/redis-10068-kavach -a <agent> -m <model> -o "$jobs"
bench/agent/harbor/check-candidates.sh
```

Build the images first (`image-tag.py build <task>`, a few minutes each; see
"Images"); a trial then takes about a minute plus the agent's time, and `check-candidates.sh` about four. `check-candidates.sh` runs the oracle on a copy
of each task for each patch in `../tasks/redis-10068/candidates/` and an empty
change, and prints the reward and Kavach's verdict for each:

| Candidate | Reward | Kavach |
| --- | --- | --- |
| `upstream`, `ms-delta-only`, `split-node-on-big-delta` | 1 | `fixed` (49 of 64 variants) |
| `stop-on-negative-delta`, empty | 0 | `invariant_violated(stream-trim-contract)@73` |

the same on both tasks.

### The model key

`KavachOpenCode` (agents/README.md) takes your own key (`OPENROUTER_API_KEY`)
and holds it in the proxy service, which forwards only `POST
/v1/chat/completions`. Still, make a dedicated OpenRouter key for benchmark
runs and give it a credit limit (OpenRouter, Settings, Keys, Credit limit):
the key is in a container next to the agent's, on a provider you may not
control, and a limit bounds what a leak or a runaway agent can spend.

### Running on a cloud provider (bring your own key)

Trials run in local Docker unless the job says otherwise; a remote provider pulls the task's prebuilt images ("Images", "Making the packages public"). The bench console's
`environment` field (`/env` in the TUI, "Where attempts run" in the web
settings) sets Harbor's `environment.type` to `daytona`, `modal` or `vercel`,
and refuses to start a run whose keys are missing. Put the keys in
`bench/agent/.env` (the console's API keys page writes it) or export them
before starting `bench`; Harbor reads them from there:

| Provider | Variables |
| --- | --- |
| `daytona` | `DAYTONA_API_KEY`, or `DAYTONA_JWT_TOKEN` and `DAYTONA_ORGANIZATION_ID` |
| `modal` | `MODAL_TOKEN_ID` and `MODAL_TOKEN_SECRET`, or a `~/.modal.toml` from `modal token new` |
| `vercel` | `VERCEL_TOKEN` (Harbor finds the team and project itself). Harbor also accepts the `.env.local` that `vercel link && vercel env pull` writes; bench does not check for it, so use the token |

On Modal, bench sets `modal_vm_runtime` (`environment.kwargs`; plain Harbor:
`--ek modal_vm_runtime=true`). Harbor's default Modal runtime forces
`network_mode: host` on every Compose service, which breaks these tasks'
own networks; the VM runtime is an alpha Modal feature with no GPU support.

A key left empty in `.env` counts as unset, because Harbor loads that file
over the environment.

Plain Harbor: `harbor run ... --env daytona --env-file bench/agent/.env`. The
provider's SDK is a Harbor extra that must be installed in Harbor's own
environment, e.g. `uv tool install --with 'harbor[daytona]' harbor` (also
`harbor[modal]`, `harbor[vercel]`); bench checks this and refuses to start
otherwise. Your key is billed by the provider; bench never prints or returns
its value.

## A trial

1. The agent works in `/work` in the agent image, as the image's user `agent`.
   In the `kavach` and `kavach-implement` arms, `task.toml` declares
   `kavach mcp` as a stdio MCP server.
2. When the agent stops, a collect hook writes its change, everything since
   the commit it started from (including any commits it made), to
   `/logs/artifacts/patch.diff`. In the arms with a contract, a second hook
   copies its `contract.json`, `rounds.jsonl`, `rounds/`, `lock.json` and
   `traffic.kavach` to `/logs/artifacts/contract/`. Then the agent's container
   is stopped.
3. The verifier runs in a new container from the grader image and gets only
   that patch. `test.sh` runs `grade.sh`: it applies the patch without changes
   to the hidden test files to the tree the agent started from, builds it,
   applies the hidden tests and runs them. The reward is 1 if they pass, else
   0. Then it runs `kavach diff` (Track A) or `kavach contract check` against
   `/task/contract` (Track B), old build against new; the `control` and
   `workflow` arms' trees get the integration first.

In the trial directory (`<jobs>/<job>/<task>__<id>/`):

| Path | |
| --- | --- |
| `result.json` | Harbor's result; the reward is `verifier_result.rewards.reward` |
| `agent/` | the agent's logs, and `trajectory.json` for agents that write one |
| `artifacts/logs/artifacts/patch.diff` | the agent's change |
| `verifier/reward.txt` | the reward |
| `verifier/kavach.json` | Kavach's verdict (below) |
| `verifier/grade.json`, `grade.log` | `grade.sh`'s result, and its build, test and Kavach output |
| `verifier/test-stdout.txt` | `test.sh`'s output |

## kavach.json

Written by the verifier in every arm. It never affects the reward.

```json
{"verdict": "fixed",
 "variants": "49 of 64 reproduce the incident on the old build · 49 of those pass on the new build",
 "old": "invariant_violated(stream-trim-contract)@73"}
```

- `verdict`: `kavach diff`'s verdict on Track A (`fixed`,
  `invariant_violated(<name>)@<seq>`, `variant_failed(<k>)@<seq>`, ...) or
  `kavach contract check`'s verdict on Track B (`pass`, `unreached@<seq>`,
  `regression@<seq>`, `invariant_violated(<name>)@<seq>`,
  `variant_failed(<k>)@<seq>`, ...), or `n/a` when it could not run.
- `features` (Track B): the `features` summary of `kavach contract check` (for
  example, `13 of 13 feature steps changed`).
- `variants`: the `variants` line of `kavach diff` or `kavach contract check`,
  or empty.
- `old`: the old build's verdict on the fixture or baseline traffic, or empty.

## What the agent can and cannot see

Checked with a stub agent that listed the container during the agent phase.

- It sees the instruction, `/work` (a fresh git repository with one commit, or
  two in the `kavach` and `kavach-implement` arms, so `git log` holds nothing
  after the base commit), `/task/TASK.md` and, in the `kavach` arm, the locked
  `/task/contract` (or `/task/incident.kavach` on Track A), the old build in
  `/opt/...` and the `kavach` CLI. `/logs/agent`, `/logs/artifacts` and an
  empty `/logs/verifier` are mounted from the trial directory.
- It cannot see the hidden test, `test.sh`, the candidates or the READMEs.
  They are only in the grader image or the task directory, and Harbor copies
  `solution/` into the container only for the oracle. Because the verifier has
  its own container, `/tests` never exists in the agent's, and nothing the
  agent leaves in its container reaches the grade except the patch.
  The verifier empties `/logs/verifier` before it runs.

## Network

Track A `task.toml` files set no network policy (`public` by default). Track
B tasks are sealed by Compose, not by Harbor's `network_mode`: each task's
`environment/docker-compose.yaml` puts `main` on an `internal: true` network
shared only with a `proxy` service (the API key lives there), so the container
reaches the model and nothing else on Docker, Daytona, Modal (only with
`--ek modal_vm_runtime=true`) and Vercel alike.
`[agent] network_mode = "no-network"` is not used: Daytona and Modal cannot
change the network of a Compose task after it starts. An agent that does not
use the proxy (Harbor's own OpenCode agent) has no model access in these
tasks. Both the design and why it replaces `network_mode` are in
[agents/README.md](agents/README.md). `sync-tasks.py` applies the Compose
block and the copy of `proxy.py` to every task; `sync-tasks.py --check` (also
run by `agents/test_agents.py`) fails when a task has drifted from it.
