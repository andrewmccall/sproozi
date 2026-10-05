# Stock CLI tools only: no Sproozi runner, completion wrapper, or credentials.
FROM node:22.16.0-bookworm@sha256:71bcbb3b215b3fa84b5b167585675072f4c270855e37a599803f1a58141a0716 AS git-build
ARG GIT_VERSION=2.50.1
ARG GIT_SHA256=7e3e6c36decbd8f1eedd14d42db6674be03671c2204864befa2a41756c5c8fc4
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates curl libcurl4-openssl-dev libexpat1-dev libssl-dev xz-utils zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
RUN curl --fail --location --output /tmp/git.tar.xz "https://www.kernel.org/pub/software/scm/git/git-${GIT_VERSION}.tar.xz" \
    && printf '%s  %s\n' "$GIT_SHA256" /tmp/git.tar.xz | sha256sum --check --status \
    && mkdir /tmp/git-src \
    && tar -xJf /tmp/git.tar.xz --strip-components=1 -C /tmp/git-src \
    && make -C /tmp/git-src -j2 prefix=/opt/git NO_GETTEXT=YesPlease NO_TCLTK=YesPlease \
    && make -C /tmp/git-src prefix=/opt/git NO_GETTEXT=YesPlease NO_TCLTK=YesPlease install

FROM node:22.16.0-bookworm@sha256:71bcbb3b215b3fa84b5b167585675072f4c270855e37a599803f1a58141a0716
ARG TARGETARCH
ARG CODEX_VERSION=0.154.0
ARG KUBECTL_VERSION=v1.34.1
ARG GH_VERSION=2.97.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libexpat1 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=git-build /opt/git /opt/git
ENV PATH="/opt/git/bin:${PATH}"
RUN npm install --global --ignore-scripts @openai/codex@$CODEX_VERSION
RUN case "$TARGETARCH" in \
      amd64) expected=7721f265e18709862655affba5343e85e1980639395d5754473dafaadcaa69e3 ;; \
      arm64) expected=420e6110e3ba7ee5a3927b5af868d18df17aae36b720529ffa4e9e945aa95450 ;; \
      *) exit 1 ;; \
    esac; \
    curl --fail --location --output /usr/local/bin/kubectl "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl" \
    && printf '%s  %s\n' "$expected" /usr/local/bin/kubectl | sha256sum --check --status \
    && chmod 0755 /usr/local/bin/kubectl
RUN case "$TARGETARCH" in \
      amd64) expected=a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112 ;; \
      arm64) expected=73ea440ecad9c9e284429997ee6f93577bc6f7bc6fba357ef62c53ad8fb641a5 ;; \
      *) exit 1 ;; \
    esac; \
    curl --fail --location --output /tmp/gh.tgz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz" \
    && printf '%s  %s\n' "$expected" /tmp/gh.tgz | sha256sum --check --status \
    && tar -xzf /tmp/gh.tgz -C /tmp \
    && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/bin/gh \
    && rm -rf /tmp/gh*
RUN useradd --uid 65532 --create-home --shell /usr/sbin/nologin agent
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["codex"]
