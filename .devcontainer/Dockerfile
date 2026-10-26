# Pinned to a patch release: tools.mod needs Go 1.26.7 or newer (buf), and a
# floating golang:1.26 tag can be months old in a local image cache. The
# image sets GOTOOLCHAIN=local, so Go would not fetch a newer toolchain.
FROM golang:1.26.10-bookworm

# git: git-svc needs 2.38 or newer (merge-tree --write-tree); bookworm ships
# 2.39. psql to inspect the git_svc schema and load migrations by hand.
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    make \
    ca-certificates \
    postgresql-client \
    locales \
    && rm -rf /var/lib/apt/lists/*

# Debian's psql is a Perl wrapper that warns on every call when the locale is
# missing, so generate en_US.UTF-8.
RUN sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen \
    && locale-gen
ENV LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

# Pinned: gopls v0.24+ needs Go 1.27. Bump with the base image.
RUN go install golang.org/x/tools/gopls@v0.23.0 \
    && go install golang.org/x/tools/cmd/goimports@v0.52.0

WORKDIR /workspace
