# Builds testdata/jobs/opencode-glm: a job of the built-in opencode agent on
# redis-10068, written in Harbor 0.24's layout from the real nop-fail job, with
# the agent logs of a real OpenCode attempt (GLM 5.3, Kavach arm; tool outputs
# trimmed, a cost of $0.002 put on each step). Trials:
#   redis-10068-kavach__Pass001   finished, reward 1, trajectory.json,
#                                 kavach.json, opencode-live.json, export
#   redis-10068-control__Run0001  running: the first 30 events of the log,
#                                 opencode-live.json without "finished"
#   redis-10068-control__Err0001  finished with an OpenCode error event and
#                                 NonZeroAgentExitCodeError, no trajectory
#   a second kavach attempt is in lock.json only: queued
#
# Regenerate, from this directory, with T the attempt's transcript.jsonl
# (bench/agent/harness/run.sh output) and harbor's own Python:
#   python3 trim.py $T full.txt && python3 trim.py $T partial.txt 30
#   $(uv tool dir)/harbor/bin/python -I atif.py full.txt trajectory.json \
#       openrouter/z-ai/glm-5.3 "XTRIM MINID deletes entries above the threshold. Fix the bug in the Redis source."
#   python3 fixture.py . ../jobs
import copy, json, os, shutil, sys
gen, td = sys.argv[1], sys.argv[2]
base = os.path.join(td, "nop-fail")
out = os.path.join(td, "opencode-glm")
shutil.rmtree(out, ignore_errors=True)
os.makedirs(out)
model = "openrouter/z-ai/glm-5.3"
jdir = "/home/user/kavach/bench/agent/results/harbor/opencode-glm"
tasks = "/home/user/kavach/bench/agent/harbor/tasks/"

def dump(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(obj, f, indent=4)

job_lock = json.load(open(os.path.join(base, "lock.json")))
tmpl = job_lock["trials"][0]
trials = []
for arm in ("control", "control", "kavach", "kavach"):
    t = copy.deepcopy(tmpl)
    t["task"]["name"] = f"redis-10068-{arm}"
    t["task"]["path"] = tasks + f"redis-10068-{arm}"
    t["agent"] = {"name": "opencode", "model_name": model, "skills": [], "resume_trajectory": False,
                  "extra_allowed_hosts": [], "kwargs": {}, "mcp_servers": []}
    trials.append(t)
job_lock["trials"] = trials
dump(os.path.join(out, "lock.json"), job_lock)
dump(os.path.join(out, "config.json"), {
    "job_name": "opencode-glm", "jobs_dir": os.path.dirname(jdir), "n_attempts": 2, "n_concurrent_trials": 1,
    "agents": [{"name": "opencode", "model_name": model, "override_timeout_sec": 3600.0}],
    "tasks": [{"path": tasks + "redis-10068-control"}, {"path": tasks + "redis-10068-kavach"}]})
dump(os.path.join(out, "result.json"), {
    "id": "7d1c2f4e-0b7a-4c55-9a51-5f1e2b8d9c10",
    "started_at": "2026-10-11T10:15:00.000000", "updated_at": "2026-10-11T04:52:00.000000Z",
    "finished_at": None, "n_total_trials": 4,
    "stats": {"n_completed_trials": 2, "n_errored_trials": 1, "n_running_trials": 1, "n_pending_trials": 1,
              "n_cancelled_trials": 0, "n_retries": 0, "evals": {},
              "n_input_tokens": None, "n_cache_tokens": None, "n_output_tokens": None, "cost_usd": None}})

base_trial = os.path.join(base, os.listdir(base)[0] if False else [d for d in os.listdir(base) if "__" in d][0])
tres = json.load(open(os.path.join(base_trial, "result.json")))

def trial(name, arm, result, files):
    d = os.path.join(out, name)
    tl = copy.deepcopy(trials[0 if arm == "control" else 2])
    dump(os.path.join(d, "lock.json"), tl)
    dump(os.path.join(d, "config.json"), {"task": {"path": tl["task"]["path"]}, "trial_name": name,
         "trials_dir": jdir, "agent": {"name": "opencode", "model_name": model}, "job_id": "7d1c2f4e-0b7a-4c55-9a51-5f1e2b8d9c10"})
    if result is not None:
        r = copy.deepcopy(tres)
        r.update(result)
        r["trial_name"] = name
        r["task_name"] = f"redis-10068-{arm}"
        r["trial_uri"] = "file://" + jdir + "/" + name
        r["agent_info"] = {"name": "opencode", "version": "1.1.8", "model_info": {"name": "z-ai/glm-5.3", "provider": "openrouter"}}
        dump(os.path.join(d, "result.json"), r)
    for rel, content in files.items():
        p = os.path.join(d, rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        if isinstance(content, str):
            shutil.copy(content, p) if os.path.exists(content) else open(p, "w").write(content)
        else:
            dump(p, content)

ses = "ses_ed6b6dacfffe25MOZ8B7cm5kqa"
trial("redis-10068-kavach__Pass001", "kavach", {
    "agent_result": {"n_input_tokens": 1968602, "n_cache_tokens": 1831936, "n_output_tokens": 34290,
                     "cost_usd": 0.082, "model_usage": None, "rollout_details": None, "metadata": None},
    "verifier_result": {"rewards": {"reward": 1.0}}, "exception_info": None,
    "started_at": "2026-10-11T04:45:30.000000Z", "finished_at": "2026-10-11T04:52:40.000000Z",
    "agent_execution": {"started_at": "2026-10-11T04:47:01.000000Z", "finished_at": "2026-10-11T04:51:10.000000Z"},
}, {
    "agent/opencode.txt": os.path.join(gen, "full.txt"),
    "agent/trajectory.json": os.path.join(gen, "trajectory.json"),
    "agent/opencode-live.json": {"url": "http://127.0.0.1:41234", "session": ses,
                                 "started": "2026-10-11T04:47:01Z", "finished": "2026-10-11T04:51:10Z"},
    "agent/opencode-session.json": {"info": {"id": ses, "title": "redis-10068"}, "messages": []},
    "verifier/reward.txt": "1\n",
    "verifier/kavach.json": {"verdict": "fixed",
        "variants": "49 of 64 reproduce the incident on the old build · 49 of those pass on the new build",
        "old": "invariant_violated(stream-trim-contract)@73"},
})
trial("redis-10068-control__Run0001", "control", None, {
    "agent/opencode.txt": os.path.join(gen, "partial.txt"),
    "agent/opencode-live.json": {"url": "http://127.0.0.1:41240", "session": "ses_running0001", "started": "2026-10-11T04:53:00Z"},
})
err_log = "".join(open(os.path.join(gen, "partial.txt")).readlines()[:4]) + json.dumps({
    "type": "error", "timestamp": 1791694030000, "sessionID": "ses_err0001",
    "error": {"name": "APIError", "data": {"message": "Provider returned error: 429 rate limited", "statusCode": 429}}}) + "\n"
trial("redis-10068-control__Err0001", "control", {
    "agent_result": {"n_input_tokens": 11200, "n_cache_tokens": 0, "n_output_tokens": 164, "cost_usd": 0.002,
                     "model_usage": None, "rollout_details": None, "metadata": None},
    "verifier_result": None,
    "exception_info": {"exception_type": "NonZeroAgentExitCodeError",
        "exception_message": "OpenCode emitted error event(s): Provider returned error: 429 rate limited",
        "exception_traceback": "Traceback (most recent call last):\n  ...\n", "occurred_at": "2026-10-11T04:44:00.000000Z"},
    "started_at": "2026-10-11T04:43:00.000000Z", "finished_at": "2026-10-11T04:44:10.000000Z",
    "agent_execution": {"started_at": "2026-10-11T04:43:20.000000Z", "finished_at": "2026-10-11T04:44:00.000000Z"},
}, {"agent/opencode.txt": err_log})
