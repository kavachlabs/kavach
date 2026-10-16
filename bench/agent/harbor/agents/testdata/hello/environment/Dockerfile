FROM ubuntu:24.04
# The container has no network once it runs, so OpenCode is installed here.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl nodejs npm ripgrep \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g opencode-ai@1.1.8 && npm cache clean --force
WORKDIR /app
