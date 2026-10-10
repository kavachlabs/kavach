#!/usr/bin/env bash
# Runs a model on a benchmark task in Docker, then grades each attempt in a
# separate container. The agent is OpenCode, the same for every model.
#
#   run.sh --task redis-10068 --arm kavach --model openrouter/<id> --key-env OPENROUTER_API_KEY
#   run.sh --task redis-10068 --arm control --model <id> \
#          --base-url https://integrate.api.nvidia.com/v1 --key-env NVIDIA_API_KEY
#
# --model is provider/model for a provider OpenCode knows. With --base-url it
# is the model's name at that OpenAI-compatible endpoint (NIM, vLLM, Z.ai...).
# A server on this machine is reachable as http://host.docker.internal:<port>.
#
# Each attempt leaves, in <out>/<task>/<model>/<arm>/<n>/:
#   transcript.jsonl  OpenCode's events (--format json), with token use
#   agent.log         OpenCode's stderr
#   patch.diff        the agent's change
#   agent.json        exit status (124 is the timeout) and wall time
#   grade.json        hidden test result and kavach diff verdict
#   grade.log         build, test and diff output
# and the OpenCode config used is <out>/<task>/opencode-<arm>.json.
set -euo pipefail

usage() { sed -n '2,20s/^# \{0,1\}//p' "$0"; exit 2; }
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
task='' arm='' model='' base_url='' runs=1 limit=3600 out="$ROOT/bench/agent/results"
keys=()
while (($#)); do
  case $1 in
    --task) task=$2 ;;
    --arm) arm=$2 ;;
    --model) model=$2 ;;
    --base-url) base_url=$2 ;;
    --key-env) keys+=("$2") ;;
    --runs) runs=$2 ;;
    --timeout) limit=$2 ;;
    --out) out=$2 ;;
    *) usage ;;
  esac
  shift 2
done
[[ -n $task && -n $model && ($arm == control || $arm == kavach) ]] || usage
dockerfile="$ROOT/bench/agent/tasks/$task/Dockerfile"
[[ -f $dockerfile ]] || { echo "no Dockerfile for task $task" >&2; exit 2; }
for k in ${keys[@]+"${keys[@]}"}; do [[ -n ${!k:-} ]] || { echo "$k is not set" >&2; exit 2; }; done
[[ -z $base_url || ${#keys[@]} -eq 1 ]] || { echo "--base-url takes exactly one --key-env" >&2; exit 2; }

image=kavach-bench/$task
for t in "$arm" grader; do
  docker build -q -f "$dockerfile" --target "$t" -t "$image:$t" "$ROOT" >/dev/null
done

# OpenCode's config is mounted outside /work, so it is not part of the
# agent's diff. It is written under $out, which must be shared with the Docker
# VM (with Colima, anything under $HOME).
mkdir -p "$out/$task"
config="$out/$task/opencode-$arm.json"
python3 - "$config" "$arm" "$model" "$base_url" ${keys[@]+"${keys[@]}"} <<'EOF'
import json, sys
path, arm, model, base_url, *keys = sys.argv[1:]
c = {
    "$schema": "https://opencode.ai/config.json",
    "autoupdate": False,
    "share": "disabled",
    # The agent must not read the upstream fix off the web. Its shell can
    # still reach the network, which the model API needs.
    # A headless run has nobody to answer "ask", so nothing may ask.
    "permission": {"edit": "allow", "bash": "allow", "webfetch": "deny",
                   "external_directory": "allow", "doom_loop": "deny"},
}
if base_url:
    c["provider"] = {"endpoint": {
        "npm": "@ai-sdk/openai-compatible",
        "name": "endpoint",
        "options": {"baseURL": base_url, "apiKey": "{env:%s}" % keys[0]},
        "models": {model: {"name": model}},
    }}
# Session titles and summaries use the small model; without this, OpenCode
# sends them to its own hosted model.
c["small_model"] = "endpoint/" + model if base_url else model
if arm == "kavach":
    c["mcp"] = {"kavach": {"type": "local", "command": ["kavach", "mcp"], "enabled": True}}
json.dump(c, open(path, "w"), indent=2)
EOF
chmod 644 "$config"
[[ -n $base_url ]] && model_arg="endpoint/$model" || model_arg=$model

env_args=()
for k in ${keys[@]+"${keys[@]}"}; do env_args+=(-e "$k"); done
slug=${model//\//_}
for ((n = 1; n <= runs; n++)); do
  dir="$out/$task/$slug/$arm/$n"
  rm -rf "$dir" && mkdir -p "$dir" && chmod 777 "$dir"
  docker run --rm --add-host host.docker.internal:host-gateway \
    ${env_args[@]+"${env_args[@]}"} -v "$config:/home/agent/.config/opencode/opencode.json:ro" -v "$dir:/out" \
    "$image:$arm" /opt/harness/agent.sh "$model_arg" "$limit"
  [[ -f $dir/patch.diff ]] || { echo "the agent container wrote nothing to $dir; is it shared with Docker?" >&2; exit 1; }
  docker run --rm -v "$dir:/out" "$image:grader" /opt/harness/grade.sh "$arm" >/dev/null
  printf '%s %s %s #%d  agent %s  grade %s\n' "$task" "$model" "$arm" "$n" "$(cat "$dir/agent.json")" "$(cat "$dir/grade.json")"
done
