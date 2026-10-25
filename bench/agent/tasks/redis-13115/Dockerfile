# syntax=docker/dockerfile:1
# Images for the redis-13115 task. Build from the repository root:
#
#   docker build -f bench/agent/tasks/redis-13115/Dockerfile --target control .
#
# Targets: control and kavach are the agent's environment for each arm;
# grader holds the hidden test. The agent images contain no hidden test, no
# candidates, no README and no Redis history after the base commit (the
# upstream fix is in it).

ARG BASE=4979cf02ff83e90ced80db5111452a4c3e082c3a

FROM golang:1.23-bookworm AS tools
RUN apt-get update && apt-get install -y --no-install-recommends cmake && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY sdk/c sdk/c
# kavach and kavach-recorder from the v0.1.4 release, installed as a user
# installs them (scripts/install.sh, served at
# https://www.thekavachlabs.com/install.sh).
COPY scripts/install.sh /tmp/install.sh
RUN KAVACH_VERSION=v0.1.4 KAVACH_INSTALL_DIR=/out/bin sh /tmp/install.sh --no-skills
RUN cmake -S sdk/c -B /tmp/c -DCMAKE_BUILD_TYPE=Release -DKAVACH_BUILD_TESTS=OFF -DKAVACH_BUILD_EXAMPLES=OFF >/dev/null \
 && cmake --build /tmp/c --target kavach_static -j4 >/dev/null \
 && mkdir -p /out/sdk/c /out/lib && cp -r sdk/c/include /out/sdk/c/ && cp /tmp/c/libkavach.a /out/lib/

FROM debian:bookworm-slim AS base
ARG BASE
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential pkg-config tcl procps git ca-certificates curl python3 nodejs npm ripgrep \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g opencode-ai@1.1.8 && npm cache clean --force
RUN useradd -m agent && mkdir -p /work /task /out && chown agent /work /out
# Daytona and Modal run commands in a login shell, whose /etc/profile resets
# PATH; this puts the image's PATH back after it.
RUN printf 'export PATH="%s"\n' "$PATH" > /etc/profile.d/00-image-path.sh
USER agent
RUN git config --global user.name agent && git config --global user.email agent@localhost \
 && git config --global init.defaultBranch main \
 && mkdir -p ~/.config/opencode ~/.local/share/opencode ~/.cache/opencode
# The base commit as a fresh repository with one commit, so the agent's
# `git diff` is its change and `git log` shows nothing from the future.
RUN curl -fsSL "https://github.com/redis/redis/archive/$BASE.tar.gz" | tar -xz --strip-components=1 -C /work \
 && cd /work && git init -q && git add -A && git commit -qm "redis $BASE"
COPY --chown=agent bench/agent/harness/agent.sh /opt/harness/agent.sh
WORKDIR /work

FROM base AS control
COPY bench/agent/tasks/redis-13115/task.md /tmp/task.md
USER root
RUN sed '/^## Kavach arm only/,$d' /tmp/task.md > /task/TASK.md && rm /tmp/task.md
USER agent
RUN make -j4 >/tmp/make.log 2>&1 && rm /tmp/make.log || (tail -50 /tmp/make.log; exit 1)

FROM base AS kavach
COPY --from=tools /out /opt/kavach
ENV PATH=/opt/kavach/bin:$PATH KAVACH_SDK=/opt/kavach/sdk/c KAVACH_LIB=/opt/kavach/lib/libkavach.a
COPY bench/agent/tasks/redis-13115/redis-kavach.patch /tmp/redis-kavach.patch
RUN git apply /tmp/redis-kavach.patch && git add -A && git commit -qm "kavach integration" \
 && make -j4 >/tmp/make.log 2>&1 && rm /tmp/make.log || (tail -50 /tmp/make.log; exit 1)
USER root
# The login shell's PATH, as in base, with the Kavach tools.
RUN printf 'export PATH="%s"\n' "$PATH" > /etc/profile.d/00-image-path.sh
RUN mkdir -p /opt/redis-old && cp /work/src/redis-server /opt/redis-old/
COPY bench/agent/tasks/redis-13115/task.md /task/TASK.md
COPY bench/agent/tasks/redis-13115/incident.kavach /task/incident.kavach
USER agent

FROM kavach AS grader
USER root
RUN git config --global --add safe.directory '*'
# One prebuilt tree per arm, at the commit the agent started from.
# Copies of /work, not git archives: the jemalloc build needs files Redis's
# .gitignore leaves out of the repository.
RUN cp -a /work /grade-kavach && cp -a /work /grade-control \
 && cd /grade-control && git reset -q --hard HEAD~1 && make distclean >/dev/null 2>&1 \
 && make -j4 >/tmp/make.log 2>&1 && rm /tmp/make.log || (tail -50 /tmp/make.log; exit 1)
COPY bench/agent/tasks/redis-13115/redis-kavach.patch /task/redis-kavach.patch
COPY bench/agent/tasks/redis-13115/hidden/test.patch /task/hidden-test.patch
COPY bench/agent/tasks/redis-13115/grade.sh /opt/harness/grade.sh
