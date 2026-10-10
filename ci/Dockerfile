# syntax=docker/dockerfile:1
# The toolchain the Linux CI jobs run in (.github/workflows/ci.yml).
# .github/workflows/toolchain-image.yml builds and publishes it;
# ci/toolchain-inputs.sh lists the pins that name a tag. A job pulls this
# one image instead of apt, the Go download site, the npm registry and the
# Playwright CDN, and .github/actions/setup only installs what the image
# lacks when a pull request changes a pin before the image is rebuilt.
#
# Steps run as uid 1001, the hosted runner's uid, so the mounted workspace
# is writable and tests that expect permission errors keep getting them.
# Caches the jobs read live at fixed paths named by ENV, because a job's
# HOME is a mount the runner provides, not /home/runner.
FROM ubuntu:24.04

ARG GO_VERSION
ARG NODE_VERSION=24.21.0
ARG PNPM_VERSION
ARG PLAYWRIGHT_VERSION
ARG GOLANGCI_LINT_VERSION
ARG GH_VERSION=2.102.0
ARG TARGETARCH=amd64

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gcc pkg-config libgtk-4-dev libwebkitgtk-6.0-dev \
        iproute2 jq kmod make procps python3 sudo tar unzip xz-utils zip zstd \
    && rm -rf /var/lib/apt/lists/* \
    && git config --system safe.directory '*' \
    && groupadd --gid 1001 runner \
    && useradd --uid 1001 --gid 1001 --create-home --shell /bin/bash runner \
    && printf 'runner ALL=(ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/runner \
    && chmod 0440 /etc/sudoers.d/runner

# The GitHub CLI, which scripts/ci-change.sh uses to read the change under
# test from the API.
RUN curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.deb" -o /tmp/gh.deb \
    && dpkg -i /tmp/gh.deb \
    && rm -f /tmp/gh.deb

# Go, pinned from go.mod. GOTOOLCHAIN stays auto so a go.mod that moves
# ahead of the image still builds while the image catches up.
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz

# Node from nodejs.org and pnpm from the registry, both pinned.
RUN arch="$TARGETARCH"; [ "$arch" = amd64 ] && arch=x64; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${arch}.tar.xz" \
        | tar -C /usr/local --strip-components=1 -xJ \
    && npm install -g "pnpm@${PNPM_VERSION}" \
    && npm cache clean --force

ENV PATH=/opt/ao-toolchain/bin:/usr/local/go/bin:$PATH \
    GOMODCACHE=/var/cache/go-mod \
    GOBIN=/opt/ao-toolchain/bin \
    pnpm_config_store_dir=/var/cache/pnpm \
    pnpm_config_cache_dir=/var/cache/pnpm-meta \
    PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright \
    AO_CI_TOOLCHAIN=1

RUN mkdir -p /opt/ao-toolchain/bin /var/cache/go-mod /var/cache/pnpm /var/cache/pnpm-meta /opt/ms-playwright \
    && chown runner:runner /opt/ao-toolchain/bin /var/cache/go-mod /var/cache/pnpm /var/cache/pnpm-meta /opt/ms-playwright

# Playwright browsers and their OS libraries for the pinned version.
RUN npx --yes "playwright@${PLAYWRIGHT_VERSION}" install-deps chromium webkit \
    && rm -rf /var/lib/apt/lists/* \
    && su runner -c "npx --yes playwright@${PLAYWRIGHT_VERSION} install chromium webkit" \
    && rm -rf /home/runner/.npm

USER runner
WORKDIR /tmp/toolchain

# The module cache for the current go.mod, the Wails CLI it names and
# golangci-lint at the Makefile's pin, all compiled by this Go. The build
# cache from compiling them is dropped: jobs restore their own.
COPY --chown=runner:runner go.mod go.sum ./
RUN go mod download \
    && go install tool \
    && go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v${GOLANGCI_LINT_VERSION}" \
    && go clean -cache

# The package store and registry metadata for the current lockfiles. A real
# install fills both and proves the lockfile and patches apply; the linked
# node_modules are dropped since each job links its own.
COPY --chown=runner:runner frontend/package.json frontend/pnpm-lock.yaml frontend/pnpm-workspace.yaml frontend/
COPY --chown=runner:runner frontend/patches frontend/patches
COPY --chown=runner:runner e2e/package.json e2e/pnpm-lock.yaml e2e/
RUN pnpm --dir frontend install --frozen-lockfile --ignore-scripts \
    && pnpm --dir e2e install --frozen-lockfile --ignore-scripts \
    && rm -rf frontend/node_modules e2e/node_modules \
    && rm -rf /home/runner/.cache /home/runner/.local

WORKDIR /
RUN rm -rf /tmp/toolchain
