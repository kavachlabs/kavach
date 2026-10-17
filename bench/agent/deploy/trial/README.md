# The public trial at try.kavachlabs.dev

`bench api -trial` on a small VM, behind a Cloudflare Tunnel. Visitors run
the agent with their own OpenRouter and Daytona or Modal keys, which stay in
the console's memory (API.md, Trial mode); invite codes run with the keys in
`/etc/kavach/trial.env`. Sandboxes run at the provider, so the VM only runs
the console and Harbor: 2 vCPU and 4 GB is enough.

The console listens on 127.0.0.1 only and cloudflared reaches it there. Its
client addresses come from `CF-Connecting-IP`, which it believes only from
loopback (`-trust-cf`); the VM needs no open port but SSH.

## Set up

1. On the VM, as root: `sh setup.sh <git ref>` (from a checkout, or fetch
   the script first). It installs Harbor with its Daytona and Modal extras
   for the `kavach` user, cloudflared, the repository at `/opt/kavach`, and
   the systemd unit.
2. Build bench for the VM and copy it over:

   ```sh
   cd bench/agent/console
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/bench-linux ./cmd/bench
   scp /tmp/bench-linux root@VM:/opt/kavach/bin/bench
   ```

   `CGO_ENABLED=0` gives a static binary, so the VM needs no Go.

3. Fill `/etc/kavach/trial.env` (see `trial.env.example`); it stays mode
   0600.
4. The tunnel, once:

   ```sh
   cloudflared tunnel login
   cloudflared tunnel create kavach-trial
   cloudflared tunnel route dns kavach-trial try.kavachlabs.dev
   cp ~/.cloudflared/<tunnel id>.json /etc/cloudflared/kavach-trial.json
   cp /opt/kavach/bench/agent/deploy/trial/cloudflared.yml /etc/cloudflared/config.yml
   cloudflared service install
   ```

5. Showcase runs: copy finished runs into `/var/lib/kavach/jobs` (for
   example `rsync -a results/harbor/<id> root@VM:/var/lib/kavach/jobs/`,
   then `chown -R kavach:kavach`) and list their ids in `TRIAL_SHOWCASE`.
6. `systemctl enable --now bench-trial`, then open https://try.kavachlabs.dev.

## Update

`git -C /opt/kavach fetch origin <ref> && git -C /opt/kavach checkout --detach FETCH_HEAD`,
copy the new bench binary beside the old one and rename it into place
(`scp /tmp/bench-linux root@VM:/opt/kavach/bin/bench.new`, then on the VM
`mv /opt/kavach/bin/bench.new /opt/kavach/bin/bench`: each run's log helper
is that executable, so it must not be overwritten in place), then
`systemctl restart bench-trial`. Runs in progress keep going; sessions are
in memory, so visitors enter their keys again.
