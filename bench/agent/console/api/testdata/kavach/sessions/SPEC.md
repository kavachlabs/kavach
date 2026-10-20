# Trial sessions survive a restart

The public trial (API.md, Trial mode) knows a visitor by a session: the
cookie `kv_session` (`__Host-kv_session` over https) holds a random id, and
the server keeps, for each id, the client address, when it was last used, the
visitor's own keys if they entered them, the invite code if they redeemed one,
and the runs they started. A run's owner record (`<jobs>/.bench/<id>.owner`)
holds the SHA-256 of its session's id; only that session may stop it, and
retry its failed attempts (pause and resume too, for a session that is not an
invite's).

Sessions live only in the server's memory, so a restart forgets them all.
Deploying the console (`kavach-trial deploy`) restarts it: after a deploy,
every visitor is a stranger, and nobody can stop or retry a run that began
before it. A partner who started a run on their invite loses its Retry
button the first time the operator ships a fix.

## What the server must do

1. A session outlives a restart. After the server starts again, a cookie it
   issued before resolves to the same session: the same invite, the same
   runs, so its owner may stop and retry them as before, and the session's
   view (`GET /api/trial/session`) shows them.
2. Keys never outlive the process. A visitor's keys are held in memory only
   and are never written to disk (API.md, "Keys"); after a restart the session
   has no keys (`has_keys` false), and the visitor enters them again. Nothing
   the server writes for a session holds a key.
3. Nothing about who owns what changes. A visitor never gains a run another
   visitor started, before or after a restart; a cookie the server did not
   issue, or one for a session that ended (replaced by a new one, or idle for
   24 hours), is no session, as now.
4. What is written for a session names neither its id nor its invite code in
   the clear: the id as its SHA-256 (as the owner records do), the invite as
   the SHA-256 of its code. A code the operator has since removed is no
   invite.
5. Nothing else changes: before any restart, every response is exactly what
   it is now, and so is a restart's effect on everything but sessions (runs
   keep going and stay readable; a run's owner record is as now).
