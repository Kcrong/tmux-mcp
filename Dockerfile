FROM alpine:3
RUN apk add --no-cache tmux
# dockers_v2 stages binaries under $TARGETPLATFORM (e.g. linux/amd64/tmux-mcp,
# linux/arm64/tmux-mcp) inside the build context, then runs a single
# `docker buildx build --platform=...` that produces a multi-arch manifest.
# `ARG TARGETPLATFORM` is automatically populated by buildx for each target
# platform, so the same Dockerfile builds the right binary into each variant.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/tmux-mcp /usr/local/bin/tmux-mcp
ENTRYPOINT ["/usr/local/bin/tmux-mcp"]
