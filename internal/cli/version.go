package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

type versionData struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
	Commit    string `json:"commit"`
	Dirty     bool   `json:"dirty"`
	SchemaMax int    `json:"schema_max"`
}

const versionCommandName = "version"

var versionCommand = &command{
	name:    versionCommandName,
	summary: "print build information and the envelope schema major",
	usage:   "ocaw version",
	run:     runVersion,
	human:   humanVersion,
}

func runVersion(c *Context, args []string) envelope.Result {
	version, commit, dirty := buildInfo()
	return envelope.Result{
		Command: versionCommandName,
		Data: versionData{
			Version:   version,
			GoVersion: runtime.Version(),
			Commit:    commit,
			Dirty:     dirty,
			SchemaMax: envelope.SchemaMajor,
		},
	}
}

func humanVersion(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(versionData)
	if !ok {
		return fmt.Errorf("version: unexpected data type %T", env.Data)
	}
	commit := data.Commit
	if data.Dirty {
		commit += "-dirty"
	}
	_, err := fmt.Fprintf(w, "ocaw %s  %s  %s  schema@%d\n",
		data.Version, data.GoVersion, commit, data.SchemaMax)
	return err
}

// buildInfo reports the module version and VCS stamp baked in at link time.
// A binary built outside a repository reports "dev" and "unknown" rather than
// failing, because version reporting must never be the reason a command breaks.
func buildInfo() (version, commit string, dirty bool) {
	version, commit = "dev", "unknown"
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return version, commit, dirty
	}
	if v := strings.TrimSpace(info.Main.Version); v != "" && v != "(devel)" {
		version = v
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if setting.Value != "" {
				commit = setting.Value
			}
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return version, commit, dirty
}
