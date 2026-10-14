# syntax=docker/dockerfile:1
# Images for the axum-1934 task. Build from the repository root:
#
#   docker build -f bench/agent/tasks/axum-1934/Dockerfile --target control .
#
# Targets: control and kavach are the agent's environment for each arm;
# grader holds the hidden test. The agent images contain no hidden test, no
# candidates, no README and no axum history after the base commit (the
# upstream fix is in it). Every crate the workspace locks is fetched and the
# focused test target is compiled, so the agent works offline and its first
# build is incremental.

ARG BASE=e97462d45251c21ffd1ba4fb5db30f3cf6f6cb6c

FROM golang:1.23-bookworm AS tools
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd cmd
COPY contract contract
COPY internal internal
COPY journal journal
COPY replay replay
COPY sdk/go sdk/go
ENV GOWORK=off CGO_ENABLED=0
RUN go build -o /out/bin/kavach ./cmd/kavach && go build -o /out/bin/kavach-recorder ./cmd/kavach-recorder
COPY sdk/rust/Cargo.toml sdk/rust/build.rs sdk/rust/README.md /out/sdk/rust/
COPY sdk/rust/src /out/sdk/rust/src

FROM rust:1.92-bookworm AS base
ARG BASE
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl python3 nodejs npm procps \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g opencode-ai@1.1.8 && npm cache clean --force
RUN useradd -m agent && mkdir -p /work /task /out && chown agent /work /out
USER agent
# Cargo's registry belongs to the agent; the toolchain stays in /usr/local.
ENV CARGO_HOME=/home/agent/.cargo CARGO_BUILD_JOBS=4
RUN git config --global user.name agent && git config --global user.email agent@localhost \
 && git config --global init.defaultBranch main \
 && mkdir -p ~/.cargo ~/.config/opencode ~/.local/share/opencode ~/.cache/opencode
# The base commit as a fresh repository with one commit, so the agent's
# `git diff` is its change and `git log` shows nothing from the future.
RUN curl -fsSL "https://github.com/tokio-rs/axum/archive/$BASE.tar.gz" | tar -xz --strip-components=1 -C /work \
 && cd /work && git init -q && git add -A && git commit -qm "axum $BASE"
# Cargo.lock is in axum's .gitignore; this one pins the versions every build
# of the task uses. It also locks the Kavach arm's driver, which the control
# arm's cargo drops.
COPY --chown=agent bench/agent/tasks/axum-1934/Cargo.lock /work/Cargo.lock
COPY --chown=agent bench/agent/harness/agent.sh /opt/harness/agent.sh
WORKDIR /work

FROM base AS control
COPY bench/agent/tasks/axum-1934/task.md /tmp/task.md
USER root
RUN sed '/^## Kavach arm only/,$d' /tmp/task.md > /task/TASK.md && rm /tmp/task.md
USER agent
RUN cargo fetch && cargo test -p axum --lib --no-run >/dev/null 2>&1
ENV CARGO_NET_OFFLINE=true

# The Kavach arm without its task files; run.sh records the committed fixture
# in it.
FROM base AS kavach-build
COPY --from=tools /out /opt/kavach
ENV PATH=/opt/kavach/bin:$PATH
COPY bench/agent/tasks/axum-1934/axum-kavach.patch /tmp/axum-kavach.patch
RUN git apply /tmp/axum-kavach.patch && git add -A && git commit -qm "kavach integration" \
 && cargo fetch --locked && cargo build -p kavach-driver >/dev/null 2>&1 \
 && cargo test -p axum --lib --no-run >/dev/null 2>&1
ENV CARGO_NET_OFFLINE=true
USER root
RUN mkdir -p /opt/axum-old && cp /work/target/debug/kavach-driver /opt/axum-old/
USER agent

FROM kavach-build AS kavach
USER root
COPY bench/agent/tasks/axum-1934/task.md /task/TASK.md
COPY bench/agent/tasks/axum-1934/incident.kavach /task/incident.kavach
USER agent

FROM kavach AS grader
USER root
RUN git config --global --add safe.directory '*'
# One prebuilt tree per arm, at the commit the agent started from. The copies
# keep target/, so dependencies are not rebuilt; the workspace's own crates
# are, since their fingerprints include the path.
RUN cp -a /work /grade-kavach && cp -a /work /grade-control \
 && cd /grade-control && git reset -q --hard HEAD~1 && git clean -qfd examples \
 && cargo test -p axum --lib --no-run >/dev/null 2>&1 \
 && cd /grade-kavach && cargo build -p kavach-driver >/dev/null 2>&1 \
 && cargo test -p axum --lib --no-run >/dev/null 2>&1
COPY bench/agent/tasks/axum-1934/axum-kavach.patch /task/axum-kavach.patch
COPY bench/agent/tasks/axum-1934/hidden/test.patch /task/hidden-test.patch
COPY bench/agent/tasks/axum-1934/grade.sh /opt/harness/grade.sh
