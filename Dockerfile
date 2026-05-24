FROM alpine:3
RUN apk add --no-cache tmux
COPY tmux-mcp /usr/local/bin/tmux-mcp
ENTRYPOINT ["/usr/local/bin/tmux-mcp"]
