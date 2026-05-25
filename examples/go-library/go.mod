module github.com/Kcrong/tmux-mcp/examples/go-library

go 1.24.6

// Use the in-tree tmux-mcp checkout so the example builds without
// publishing a new module version. Drop this line and `go get` the
// real module to build against a release.
replace github.com/Kcrong/tmux-mcp => ../..

require github.com/Kcrong/tmux-mcp v0.0.0-00010101000000-000000000000
