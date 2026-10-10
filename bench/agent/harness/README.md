# Agent harness

Runs a model on a benchmark task in Docker and grades each attempt. Every
model gets the same agent (OpenCode 1.1.8), the same tools and the same
prompt; only the model changes. That is what lets open-weight and frontier
models be compared, with and without Kavach.

## Running

Needs Docker (on macOS, Colima works: `brew install colima docker docker-buildx`,
then `colima start --cpu 6 --memory 8`) and python3. Colima shares only
`$HOME` with its VM, so `--out` must be under it (the default is
`bench/agent/results/`).

```sh
# A provider OpenCode knows (OpenRouter, Anthropic, ...): provider/model
export OPENROUTER_API_KEY=...
bench/agent/harness/run.sh --task redis-10068 --arm kavach \
  --model openrouter/<model-id> --key-env OPENROUTER_API_KEY --runs 3

# Any OpenAI-compatible endpoint: NVIDIA's hosted NIM API, a self-hosted NIM
# or vLLM server, Z.ai. --model is the name the endpoint uses.
export NVIDIA_API_KEY=...
bench/agent/harness/run.sh --task redis-10068 --arm control \
  --model <model-id> --base-url https://integrate.api.nvidia.com/v1 \
  --key-env NVIDIA_API_KEY --runs 3
```

A server on the host, such as a local NIM container, is
`--base-url http://host.docker.internal:8000/v1`.

Each attempt prints one line and leaves its files in
`bench/agent/results/<task>/<model>/<arm>/<n>/` (see the header of `run.sh`).
The grade is `hidden` in `grade.json`: `pass` when the hidden upstream test
passes. `kavach_diff` is recorded beside it but does not grade.

## How a run works

1. `run.sh` builds the task's images from `tasks/<task>/Dockerfile`:
   - `control`: the repository at the base commit, built;
   - `kavach`: the same plus the integration, committed, the `kavach` CLI, the
     fixture and the build that failed;
   - `grader`: the `kavach` image plus the hidden test.
2. The agent container runs `opencode run` on `/task/TASK.md` in `/work`, as
   a non-root user, until OpenCode exits or the timeout (default one hour).
   In the Kavach arm, `kavach mcp` is configured as an MCP server.
3. The agent's change is `git diff` of `/work` against the commit it started
   from.
4. A fresh grader container applies the change to the same starting tree,
   without changes under `tests/`, builds it, runs the hidden test, then runs
   `kavach diff` on the fixture. The control arm's tree gets the integration
   first; if that does not apply, `kavach_diff` is `n/a`.

## What the agent cannot see

- The hidden test, the candidate fixes and the task README are only in the
  grader image or not in any image.
- The source is a fresh git repository with one commit (two in the Kavach
  arm), so `git log` holds nothing after the base commit.
- OpenCode's web fetch tool is denied.

The container has network access, which the model API needs, so the agent's
shell could still download the upstream fix. Transcripts should be checked for
it until egress is limited to the model endpoint.
