# Contributing to Kavach

Thanks for helping. A few rules keep this project fast to review.

## Sign your commits (DCO)

Every commit must carry a `Signed-off-by` line certifying the
[Developer Certificate of Origin](DCO). There is no CLA.

```bash
git commit -s -m "replay: report divergence offset"
```

CI rejects pull requests whose commits are not signed off. To fix a branch,
run `git rebase --signoff origin/main` and force-push it.

## Keep pull requests small

One change per pull request. If a change needs a design discussion, open an
issue or a GitHub Discussion first.

## AI-assisted contributions

AI-assisted pull requests are welcome. Say so in the description and name the
tool. You are still the author: you must understand the change, have run the
tests, and be able to answer review questions about it.

## Bug reports

Once the flight recorder ships, every bug report must include a Kavach fixture
that reproduces the problem (`kavach inspect` output is fine if the fixture
contains sensitive data). A report without one may be closed with a request
for one.

## Format changes

Changes to the journal format go through [SPEC.md](SPEC.md) first, follow its
compatibility rules, and must come with conformance fixtures.

## Response times

We aim to answer every new issue and pull request within 24 hours.
