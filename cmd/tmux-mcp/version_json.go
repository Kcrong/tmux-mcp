package main

import (
	"encoding/json"
	"io"
	"runtime/debug"
)

// versionJSONInfo is the schema printed by -version-json. Field names are
// stable, lowercase, and JSON-tagged so downstream tooling can rely on
// them.
type versionJSONInfo struct {
	Version string `json:"version"`
	Go      string `json:"go"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// emitVersionJSON writes the JSON metadata to out and returns any encode
// error. Reads vcs.* fields from runtime/debug.ReadBuildInfo when set
// (they are populated when the binary is built with -trimpath via go
// build); falls back to empty strings otherwise.
func emitVersionJSON(out io.Writer, version, goVersion string) error {
	info := versionJSONInfo{
		Version: version,
		Go:      goVersion,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Commit = s.Value
				if len(info.Commit) > 7 {
					info.Commit = info.Commit[:7]
				}
			case "vcs.time":
				info.Date = s.Value
			}
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "")
	return enc.Encode(info)
}
