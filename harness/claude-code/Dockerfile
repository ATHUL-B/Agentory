# Agentory harness: Claude Code, run once per Task by the operator's JobBackend.
FROM node:24.22.1-slim

ARG CLAUDE_CODE_VERSION=2.1.311
RUN apt-get update \
 && apt-get install -y --no-install-recommends git python3 ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && npm cache clean --force

COPY runner.py /usr/local/lib/agentory/runner.py

# The image's non-root "node" user; the operator mounts writable /workspace and /tmp.
USER 1000:1000
ENV HOME=/tmp/home AGENTORY_WORKSPACE=/workspace
WORKDIR /workspace
ENTRYPOINT ["python3", "/usr/local/lib/agentory/runner.py"]
