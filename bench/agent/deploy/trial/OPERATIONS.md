# Running the public trial

Day-to-day upkeep of https://try.kavachlabs.dev. Setting it up is in
[README.md](README.md). Partners' invite codes, tasks, models and console
releases go through the `kavach-trial` CLI from your laptop
([ONBOARDING.md](ONBOARDING.md)); this file is the rest.

The VM: Hetzner CPX12 (1 vCPU, 2 GB RAM, 2 GB swap), Ubuntu, root SSH by
key, firewall open on TCP 22 only. The site reaches it through a Cloudflare
Tunnel: `cloudflared` → `bench-trial` on 127.0.0.1:8480, running as user
`kavach`. Runs live in `/var/lib/kavach/jobs`; keys in
`/etc/kavach/trial.env` (root, 0600); invite codes, tasks, models and the
showcase list in `/etc/kavach/trial.json` (root:kavach, 0640). The codes in
`trial.json` are as good as the keys: keep it 0640 and out of chat, issues
and backups you would share.

Locked out of SSH: Hetzner Cloud Console → the server → **Console** (>_)
is a terminal on the VM that needs no SSH. It logs in with root's password;
a server made with an SSH key has none, so set one under **Rescue** →
**Reset root password** first. From there, `fail2ban-client set sshd unbanip
<ip>` lifts a ban, and removing
`/etc/ssh/sshd_config.d/00-kavach-hardening.conf` then
`systemctl reload ssh` undoes the SSH hardening.

| Unit | What it is |
| --- | --- |
| `bench-trial.service` | the console |
| `cloudflared.service` | the tunnel; stopping it takes the site offline |
| `bench-retention.timer` | deletes old runs daily (`retention.sh`) |
| `unattended-upgrades`, `fail2ban` | security updates, SSH bans (`harden.sh`) |

## Live runs

Each run is a Harbor process (or bench's log helper in front of one) that
outlives the console. A run is live while a process's command line names
`/var/lib/kavach/jobs/.bench/<id>.json` or ends with
`/var/lib/kavach/jobs/<id>`; this is what the console checks
(`harbor/run.go`, `Runner.pid` and `Runner.orphans`). To see them:

```sh
ps -eo pid,etime,args | grep -F /var/lib/kavach/jobs/ | grep -v grep
```

No output means no live runs. From your laptop, `kavach-trial status` shows
the service, the checkout, a live-run count and invite usage. The console's view (it also lists a folder
with no `result.json` as running, so trust `ps` when they disagree):

```sh
curl -s http://127.0.0.1:8480/api/jobs | jq -r '.[] | select(.status == "running") | .id'
```

A run's Harbor output is `/var/lib/kavach/jobs/.bench/<id>.log`.

To retry attempts of a run started on an invite (a provider error, a hung
model call) without the visitor's browser, from your laptop:

```sh
kavach-trial retry calm-bronze-toucan-jldl3zs 8aEvUJy 2aHR4Na
```

An attempt is its folder name or the part after `__`. Each prints `started`,
or `queued` when the run is still going: it starts when the run ends. It
runs on the server's keys, as the run did, and skips the visitor's limits
and retry cap. A run on a visitor's own keys can't be retried this way; the
server keeps no keys for it. This talks to the service over
`/var/lib/kavach/admin.sock` (`-admin-socket` in `bench-trial.service`,
which `kavach-trial deploy` installs from the checkout it deploys).

## Upkeep

- **Security updates** install by themselves (Ubuntu's `-security` pocket
  only) and never reboot. Check for a pending reboot about weekly:
  `cat /var/run/reboot-required.pkgs 2>/dev/null`. Everything else,
  including cloudflared from Cloudflare's repository, waits for you:
  `apt-get update && apt list --upgradable`, then
  `NEEDRESTART_MODE=l apt-get upgrade` with no live runs (needrestart then
  lists services to restart instead of restarting bench-trial or
  cloudflared itself).
- **The console**: `kavach-trial deploy [ref]` (ONBOARDING.md, "Ship a
  console change"). It refuses while runs are live, checks the new binary
  against `trial.json` first and rolls back on a failed restart. A restart
  leaves runs and sessions going but forgets visitors' keys. A push to
  main that changes the console or the root packages it builds in deploys
  itself: `.github/workflows/trial-deploy.yml` waits up to an hour for no
  live runs, then runs `kavach-trial deploy` on that commit. Run it by hand
  from the Actions tab (workflow_dispatch) to redeploy main.
- **Disk**: `df -h /` and `du -sh /var/lib/kavach/jobs`. The journal is
  capped at 200M (`journalctl --disk-usage`).
- **Memory**: `free -h`. Each live run is a Harbor process on 2 GB;
  `TRIAL_MAX_RUNNING` (default 3) bounds them.
- **SSH bans**: `fail2ban-client status sshd`; unban with
  `fail2ban-client set sshd unbanip <ip>`. Loopback is never banned. If
  your own address is banned, use the Hetzner console (above) or SSH from
  another address.
- **Hardening again** (after changing SSH or on a VM set up before
  `harden.sh`): `sh /opt/kavach/bench/agent/deploy/trial/harden.sh`, then
  log in from a new terminal before closing the old one.

## Safe reboot

A reboot kills live runs, and their sandboxes stay up at Daytona or Modal
until the provider stops them. So stop new runs first and let the others
finish:

1. `systemctl stop cloudflared` (the site is offline; live runs keep
   going, they need only outbound access).
2. Wait until the `ps` line in [Live runs](#live-runs) prints nothing. To
   cut it short, stop a run from the console before step 1, or
   `kill -INT <pid>` (Harbor then tears its sandboxes down).
3. `systemctl reboot`.
4. After it comes back: `systemctl is-active bench-trial cloudflared` and
   `systemctl list-timers bench-retention.timer`, then open the site.

## Retention

`retention.sh` deletes a run (its Harbor folder and its files in `.bench/`)
when nothing in either has changed for 30 days, except showcase runs
(listed in `trial.json`'s `showcase` or in `TRIAL_SHOWCASE`; both count) and
live runs. It also removes `.bench/home/<id>` folders older than a day whose
run is not live. It touches only folders with a plain name
(`[A-Za-z0-9._-]`) that hold Harbor's `config.json`, `lock.json` or
`result.json`, never the console's own files (`invites-used.json`, ...).
Every deletion is logged; a folder it cannot delete is logged, kept with its
bookkeeping, and makes the pass exit 1 (the unit shows as failed).

The timer's service runs as `kavach` and does not load `trial.env`: root
copies out only its `TRIAL_SHOWCASE` line before the script starts. It
reads `trial.json` itself, so that file must stay readable by `kavach`; if
it cannot be read or parsed, nothing is deleted and the unit fails.

```sh
sh /opt/kavach/bench/agent/deploy/trial/retention.sh --dry-run   # as root: lists what it would delete
systemctl enable --now bench-retention.timer                     # once, after the dry run looks right
journalctl -u bench-retention                                    # what it deleted
systemctl start bench-retention                                  # run it now
```

Keep runs longer with `systemctl edit bench-retention` and

```ini
[Service]
Environment=RETENTION_DAYS=60
```

or `--days N` by hand. To keep a run for good, add its id to `showcase` in
`trial.json` (by hand; `kavach-trial` does not edit that list) before it
turns 30 days old.

## The benchmark site

benchmark.kavachlabs.dev is a read-only copy of chosen runs (`bench export`,
console API.md "A read-only copy"), served from the R2 bucket
`kavach-bench-site`. From the kavach checkout, with wrangler logged in to the
account:

```sh
sh bench/agent/deploy/trial/kavach-trial publish --dry-run <run>...   # export and check only
sh bench/agent/deploy/trial/kavach-trial publish [--landing ID] <run>...
```

Name every run the site should hold, not only new ones: the site is exported
again from all of them. A run comes from your local results
(`KAVACH_BENCH_JOBS`, default `bench/agent/results/harbor`) if it is there,
else it is copied from the VM into `~/.cache/kavach-trial/jobs`. Publish
refuses to upload anything shaped like a key, but transcripts and patches go
out as they are, so read the dry run's copy in `~/.cache/kavach-trial/site`
first. Each run copied from the VM is then added to the showcase
(`kavach-trial showcase add|remove|list`), so retention keeps it. Files the
new copy no longer has stay in the bucket, unreachable from its pages.

To ship a change to the console's web app without exporting the runs again:

```sh
sh bench/agent/deploy/trial/kavach-trial publish --app-only [--landing ID]
```

It uploads only the app's files, and opens on the run the live site opens on
unless `--landing` names another. The trial-deploy workflow runs it after a
deploy whose push changed `bench/agent/console/web`, with the
`CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` secrets in place of a
wrangler login.

## Monitoring

`.github/workflows/trial-uptime.yml` fetches `/api/trial/session` every 15
minutes and expects 200 with JSON that has `limits`; a probe fails after
three tries 20 s apart, and the run then fails. Two failed probes in a row
open an issue labelled `trial-down`; while it stays down the issue gets a
comment at most every 6 hours; the next good probe closes it with a
comment. GitHub runs
schedules only from the default branch, so it starts once it is on `main`;
run it by hand from Actions → trial-uptime → Run workflow. GitHub turns off
scheduled workflows in a repository with no activity for 60 days; re-enable
it from the same page. A 403 with a Cloudflare challenge page in the issue
means the zone's bot protection stopped the runner, not that the trial is
down.

On the VM: `journalctl -u bench-trial -f`, `journalctl -u cloudflared -f`.

## Cloudflare rate limit

The console limits writes itself (20 a minute per address, `RatePerMin`).
This rule drops floods at Cloudflare before they reach the 1-vCPU VM.

What a plan allows ([Cloudflare docs](https://developers.cloudflare.com/waf/rate-limiting-rules/#availability),
checked 2026-10-18):

| Plan | Rules | Fields in the expression | Period | Block for | Counted by |
| --- | --- | --- | --- | --- | --- |
| Free | 1 | URI path, verified bot | 10 s | 10 s | IP |
| Pro | 2 | adds host, URI, full URI, query | up to 1 min | up to 1 h | IP |
| Business | 5 | adds method, source IP, user agent | up to 10 min | up to 1 day | IP |

The rule as intended, host `try.kavachlabs.dev`, any method but GET, path
under `/api/`, needs **Business** (Pro has no method, Free no host). On
**Free**, the rule can only match paths, and it then applies to every host
in the kavachlabs.dev zone, so it names the trial's write paths exactly.
GETs to them count too; the page fetches `/api/jobs` only on load and after
a reconnect, so 15 in 10 s is far above a visitor's use.

| | Free (use this unless the zone is on Business) | Business or higher |
| --- | --- | --- |
| Expression | `(http.request.uri.path in {"/api/trial/keys" "/api/trial/invite" "/api/jobs"})` | `(http.host eq "try.kavachlabs.dev" and http.request.method ne "GET" and starts_with(http.request.uri.path, "/api/"))` |
| Counting | IP | IP |
| Rate | 15 requests per 10 s | 30 requests per 1 min |
| Action | Block (429) for 10 s | Block (429) for 10 min |

`POST /api/jobs/<id>/stop` is left out of the Free rule: a path pattern
needs a field function a Free zone may not accept, and stopping is cheap.

### Dashboard

1. dash.cloudflare.com → the **kavachlabs.dev** zone → **Security** →
   **Security rules** (older dashboards: **Security** → **WAF** →
   **Rate limiting rules**).
2. Look at the existing rate limiting rules first. A Free zone has room for
   one; if another site already uses it, stop and decide which matters
   more.
3. **Create rule** → **Rate limiting rule**. Name:
   `try.kavachlabs.dev API writes`.
4. **If incoming requests match**: Free: field *URI Path*, operator *is in*,
   values `/api/trial/keys`, `/api/trial/invite`, `/api/jobs`. Business:
   *Edit expression* and paste the Business expression above.
5. **With the same characteristics**: IP.
6. **When rate exceeds**: Free 15 requests per 10 seconds; Business 30 per
   1 minute.
7. **Then take action**: Block, default response; duration Free 10 seconds,
   Business 10 minutes.
8. **Deploy**. Rate limiting rules run after custom rules; leave its place
   in the list as it is.

### API

A token with **Zone → Zone WAF → Edit** on kavachlabs.dev; the zone id is on
the zone's Overview page. This reads the zone's `http_ratelimit` entry
point first, then updates our rule in place if it is there, appends it if
not, and creates the entry point only if the zone has none, so the zone's
other rules stay.

```sh
export CF_API_TOKEN=... ZONE_ID=...
API=https://api.cloudflare.com/client/v4
AUTH="Authorization: Bearer $CF_API_TOKEN"
NAME='try.kavachlabs.dev API writes'

# Free plan. On Business, use instead:
#   expression (http.host eq "try.kavachlabs.dev" and http.request.method ne "GET" and starts_with(http.request.uri.path, "/api/"))
#   period 60, requests_per_period 30, mitigation_timeout 600
RULE=$(jq -n --arg d "$NAME" '{
  description: $d,
  expression: "(http.request.uri.path in {\"/api/trial/keys\" \"/api/trial/invite\" \"/api/jobs\"})",
  action: "block",
  ratelimit: {
    characteristics: ["cf.colo.id", "ip.src"],
    period: 10,
    requests_per_period: 15,
    mitigation_timeout: 10
  },
  enabled: true
}')

# 1. The entry point, and what is in it now.
code=$(curl -sS -o entrypoint.json -w '%{http_code}' -H "$AUTH" \
  "$API/zones/$ZONE_ID/rulesets/phases/http_ratelimit/entrypoint")
echo "HTTP $code"
jq '{id: .result.id, rules: [.result.rules[]? | {id, description, expression, ratelimit}]}' entrypoint.json

# 2. Update, append or create.
if [ "$code" = 200 ]; then
  RULESET_ID=$(jq -r '.result.id' entrypoint.json)
  RULE_ID=$(jq -r --arg d "$NAME" '.result.rules[]? | select(.description == $d) | .id' entrypoint.json)
  if [ -n "$RULE_ID" ]; then
    curl -sS -X PATCH -H "$AUTH" -H 'Content-Type: application/json' --data "$RULE" \
      "$API/zones/$ZONE_ID/rulesets/$RULESET_ID/rules/$RULE_ID" | jq '{success, errors}'
  else
    curl -sS -X POST -H "$AUTH" -H 'Content-Type: application/json' --data "$RULE" \
      "$API/zones/$ZONE_ID/rulesets/$RULESET_ID/rules" | jq '{success, errors}'
  fi
elif [ "$code" = 404 ]; then
  jq -n --argjson r "$RULE" '{name: "default", kind: "zone", phase: "http_ratelimit", rules: [$r]}' |
    curl -sS -X POST -H "$AUTH" -H 'Content-Type: application/json' --data @- \
      "$API/zones/$ZONE_ID/rulesets" | jq '{success, errors}'
else
  echo "stopping: unexpected answer"; cat entrypoint.json
fi
```

`cf.colo.id` is required in `characteristics` through the API (the
dashboard adds it for you). On a Free zone whose one rule is taken, the
POST fails with a quota error and changes nothing. Check it took:
`curl -sI -X POST https://try.kavachlabs.dev/api/trial/invite` sixteen times
within 10 s; the last ones answer 429 from Cloudflare.

## Spend caps

Visitors' own keys are their own spend. These cap the operator's keys in
`trial.env`, which invite codes use. Checked 2026-10-18.

**OpenRouter** (`OPENROUTER_API_KEY`), a credit limit on that key:

1. openrouter.ai → **Settings** → **API Keys** (openrouter.ai/settings/keys).
2. Create a key just for the trial, or edit the one in `trial.env`.
3. Set **Credit limit** in USD and its reset (daily, weekly, monthly or
   none; resets are at midnight UTC, weeks Monday to Sunday). Leave BYOK
   usage counted. Save. A key at its limit gets errors instead of spending.

By API, with a management key: list keys and their `hash` with
`GET https://openrouter.ai/api/v1/keys`, then

```sh
curl -sS -X PATCH "https://openrouter.ai/api/v1/keys/$KEY_HASH" \
  -H "Authorization: Bearer $OPENROUTER_MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  -d '{"limit": 25, "limit_reset": "daily", "include_byok_in_limit": true}'
```

Check from the VM with the key itself:
`curl -s https://openrouter.ai/api/v1/key -H "Authorization: Bearer $OPENROUTER_API_KEY" | jq '.data | {limit, limit_remaining, limit_reset}'`.

**Daytona** (`DAYTONA_API_KEY`) has no dollar budget. What bounds it:

1. A separate Daytona organization for the trial, so its key reaches only
   the trial's sandboxes.
2. **Billing** → **Wallet** (app.daytona.io/dashboard/billing/wallet) →
   **Automatic top-up**: set both *when balance is low* and *bring balance
   to* to 0 and **Save**. The prepaid balance is then the most it can
   spend. Top up by hand.
3. **Limits** (app.daytona.io/dashboard/limits) shows the organization's
   tier: its vCPU, memory and disk across all running sandboxes (Tier 1:
   10 vCPU, 10 GiB, 30 GiB). That caps how much runs at once. Lower custom
   limits: ask support@daytona.io.
4. **Sandbox spending** in the dashboard shows cost per sandbox and month.

Daytona does not document what happens at a zero balance; ask support
before relying on it.

**Modal** (only if you add operator Modal tokens to `trial.env`), a
workspace budget:

1. Use a workspace just for the trial.
2. modal.com → **Settings** → **Usage & Billing** (modal.com/settings/usage)
   → set the workspace budget. Only workspace Owners and Managers can; the
   highest budget allowed grows with past successful charges.
3. At the limit Modal stops workloads that would incur more charges.
   On Team and Enterprise plans an environment can have its own budget
   (Settings → Workspace Management → Environments), no higher than the
   workspace's.

## Incidents

**Take the trial offline now**: `systemctl stop cloudflared`. The site
answers with a Cloudflare error; the VM and live runs keep going. Back
online: `systemctl start cloudflared`. To also stop runs being started from
anything already loaded in a browser, `systemctl stop bench-trial` (live
runs keep going; stop them with `kill -INT <pid>` from [Live runs](#live-runs)).

**Site down** (a `trial-down` issue, or the page does not load):

1. `systemctl status bench-trial cloudflared`.
2. `journalctl -u bench-trial -n 200 --no-pager`, and the same for
   `cloudflared`.
3. `curl -s http://127.0.0.1:8480/api/trial/session | jq .limits`: if this
   works, the console is fine and the tunnel or Cloudflare is not.
4. `systemctl restart bench-trial` (live runs and sessions survive it; keys do not).

**Abuse or a cost spike**: take it offline, then look at what is running
([Live runs](#live-runs)) and which runs one visitor started: `.bench/<id>.owner`
holds a digest of the session, the same for all of its runs. The console
does not log client addresses; Cloudflare's Security → Events and
Analytics show them, and a custom rule can block one. Revoke or lower the
provider caps above. Revoke an invite code with
`kavach-trial invite revoke <code|label>`, which takes effect at once
without a restart (ONBOARDING.md); see which runs a code started with
`kavach-trial invite show`. Rotate a leaked key at the provider,
then in `trial.env`, then `systemctl restart bench-trial`. If `trial.json`
leaked, revoke every code in it (`kavach-trial invite list`, then `revoke`)
and hand out new ones.

**Disk full**: `sh .../retention.sh --dry-run --days 7`, then without
`--dry-run`; `journalctl --vacuum-size=100M`.
