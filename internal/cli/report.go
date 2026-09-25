package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

const reportCommandName = "report"

// reportFormats. md is the only way to get anything other than the envelope on
// stdout, so it is spelled out rather than inferred.
const (
	reportFormatJSON = "json"
	reportFormatMD   = "md"
)

var reportFormats = []string{reportFormatJSON, reportFormatMD}

// reportData is the payload. `status` is the drift verdict the file was in
// before this command touched it, which is the fact `--write` is usually run to
// learn.
type reportData struct {
	Format     string   `json:"format"`
	Path       string   `json:"path"`
	Written    bool     `json:"written"`
	Bytes      int      `json:"bytes"`
	Markdown   string   `json:"markdown,omitempty"`
	Prev       string   `json:"previous_status"`
	Status     string   `json:"status"`
	Drifted    bool     `json:"drifted"`
	Tasks      int      `json:"tasks"`
	Acceptance int      `json:"acceptance"`
	Mode       string   `json:"mode"`
	DryRun     bool     `json:"dry_run"`
	Notes      []string `json:"notes,omitempty"`
}

// reportFlagsState is package state, so it is reset by setup for the same reason
// init's is: flag.BoolVar takes the current value as the flag's default, so a
// second invocation in one process would inherit the first one's --write.
var reportFlagsState struct {
	write  bool
	format string
}

var reportCommand = &command{
	name:    reportCommandName,
	summary: "render the workflow state as markdown, or rewrite the file",
	usage:   "ocaw report [--write] [--format <md|json>] [--output <path>]",
	human:   humanReport,
	setup: func(fs *flag.FlagSet, c *Context) {
		reportFlagsState.write = false
		reportFlagsState.format = ""
		fs.BoolVar(&reportFlagsState.write, "write", reportFlagsState.write,
			"regenerate WORKFLOW_STATE.md; the repair for render_drift")
		fs.StringVar(&reportFlagsState.format, "format", reportFlagsState.format,
			"md emits the rendered markdown, json emits the envelope (default)")
	},
	run: runReport,
}

func runReport(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: reportCommandName}
	if len(args) > 0 {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw report --write",
			"unexpected argument %q", args[0],
		)
		return res
	}
	format := reportFlagsState.format
	if format == "" {
		format = reportFormatJSON
	}
	if !contains(reportFormats, format) {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw report --format md",
			"--format %q is not one of %s", format, strings.Join(reportFormats, "|"),
		)
		return res
	}

	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		res.Err = asEnvelopeError(err)
		return res
	}
	if missing := layout.Require(); missing != nil {
		res.Err = missing
		return res
	}

	data, warnings, resErr := reportRun(c, layout, format)
	res.Data = data
	res.Warnings = warnings
	if resErr != nil {
		res.Err = resErr
		return res
	}

	// `--format md` is the only way to get something other than the envelope on
	// stdout. It is an explicit request, so it is honoured in a non-TTY as well:
	// `ocaw report --format md > file.md` is the documented way to get a copy,
	// and making that require a terminal would be a strange rule. Every other
	// combination still follows §4.1, including --quiet and --output.
	if format == reportFormatMD && resErr == nil && c.Options.Output == "" && c.mode() != ModeQuiet {
		if _, werr := io.WriteString(c.Stdout, data.Markdown); werr != nil {
			res.Data = nil
			res.Err = writeFailure("write the markdown to stdout", werr)
		}
	}
	return res
}

func reportRun(c *Context, layout *workspace.Layout, format string) (reportData, []envelope.Warning, *envelope.Error) {
	data := reportData{
		Format: format,
		Path:   layout.Rel(layout.StateMDPath()),
		Prev:   string(report.StatusMissing),
		DryRun: c.Options.DryRun,
		Notes:  []string{},
		Mode:   c.mode().String(),
	}

	var warnings []envelope.Warning
	release := func() {}
	if reportFlagsState.write {
		// --write rewrites the file, so it is a mutation and takes the lock. A
		// read-only `report` does not, and failing with lock_held because
		// someone else is working would be a strange answer to "what does the
		// file say".
		lock, lockWarnings, lockErr := workspace.Acquire(layout)
		if lockErr != nil {
			return data, nil, asEnvelopeError(lockErr)
		}
		warnings = lockWarnings
		release = func() {
			if relErr := lock.Release(); relErr != nil {
				warnings = append(warnings, Warn(envelope.CodeLockHeld,
					"could not release the workspace lock at %s: %v", lock.Path(), relErr))
			}
		}
	}
	defer func() { release() }()

	st, loadErr := state.Load(layout)
	if loadErr != nil {
		return data, warnings, asEnvelopeError(loadErr)
	}
	data.Tasks = len(st.Tasks)
	data.Acceptance = len(st.Workflow.Acceptance)

	previous := report.StatusMissing
	if doc, found, derr := report.Load(layout); derr != nil {
		return data, warnings, asEnvelopeError(derr)
	} else if found {
		if status, _ := report.Check(doc, st); status != "" {
			previous = status
		}
	}
	data.Prev = string(previous)
	data.Drifted = previous != report.StatusCurrent && previous != report.StatusMissing

	markdown := report.Render(st)
	data.Markdown = markdown
	data.Bytes = len(markdown)
	data.Status = string(report.StatusCurrent)

	if !reportFlagsState.write {
		if previous == report.StatusMissing {
			data.Notes = append(data.Notes,
				"WORKFLOW_STATE.md does not exist; this is what `ocaw report --write` would write")
		}
		return data, warnings, nil
	}

	if c.Options.DryRun {
		data.Notes = append(data.Notes, "dry run: the file was not written")
		return data, warnings, nil
	}
	if _, werr := report.Write(layout, st); werr != nil {
		return data, warnings, writeFailure("write "+data.Path, werr)
	}
	data.Written = true
	if previous == report.StatusEdited {
		data.Notes = append(data.Notes,
			"the previous file had been edited by hand; state.json is the source of truth, so those edits are gone")
	}
	return data, warnings, nil
}

func humanReport(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(reportData)
	if !ok {
		return fmt.Errorf("report: unexpected data type %T", env.Data)
	}
	if data.Format == reportFormatMD {
		// The markdown already went to stdout; anything more would be noise on
		// top of the document the caller asked for.
		return nil
	}
	if data.Written {
		if _, err := fmt.Fprintf(w, "wrote %s (%d bytes, was %s)\n", data.Path, data.Bytes, data.Prev); err != nil {
			return err
		}
	} else if data.DryRun {
		if _, err := fmt.Fprintf(w, "would write %s (%d bytes, was %s)\n", data.Path, data.Bytes, data.Prev); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "%s is %s\n", data.Path, data.Prev); err != nil {
			return err
		}
		if data.Drifted {
			if _, err := fmt.Fprintf(w, "  repair with ocaw report --write\n"); err != nil {
				return err
			}
		}
	}
	for _, note := range data.Notes {
		if _, err := fmt.Fprintf(w, "  note  %s\n", note); err != nil {
			return err
		}
	}
	return nil
}
