package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/doctor"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

const doctorCommandName = "doctor"

type doctorFlags struct {
	strict  bool
	fixSafe bool
}

var doctorOpts doctorFlags

var doctorCommand = &command{
	name:    doctorCommandName,
	summary: "check workspace health and report every finding",
	usage:   "ocaw doctor [--strict] [--fix-safe]",
	setup: func(fs *flag.FlagSet, c *Context) {
		doctorOpts = doctorFlags{}
		fs.BoolVar(&doctorOpts.strict, "strict", doctorOpts.strict,
			"treat warnings as errors, for a CI gate")
		fs.BoolVar(&doctorOpts.fixSafe, "fix-safe", doctorOpts.fixSafe,
			"apply the reversible fixes: missing directories, a drifted WORKFLOW_STATE.md, knowledge entries whose anchor is gone")
	},
	run:   runDoctor,
	human: humanDoctor,
	// A non-zero exit here is a verdict about the workspace, not a failure of
	// the command, and the findings are the whole point of running it.
	humanOnError: true,
}

func runDoctor(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: doctorCommandName}

	// doctor is the one command that must work on a broken workspace, so it does
	// not use layout.Require(). A missing .agent/ is a finding, not a reason to
	// stop: the rest of the checks still have things to say, and a health check
	// that reports one problem per run is one nobody runs twice.
	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		res.Err = asEnvelopeError(err)
		return res
	}

	rep := doctor.Run(layout, doctor.Options{
		Strict:  doctorOpts.strict,
		FixSafe: doctorOpts.fixSafe,
		DryRun:  c.Options.DryRun,
	})
	data := doctorData{
		Findings:  rep.Findings,
		Fixed:     rep.Fixed,
		Counts:    rep.Counts(),
		Checks:    ranChecks(rep),
		VerifyCmd: rep.VerifyCmd,
		Strict:    doctorOpts.strict,
		FixSafe:   doctorOpts.fixSafe,
		DryRun:    c.Options.DryRun,
	}
	if data.Fixed == nil {
		data.Fixed = []string{}
	}
	res.Data = data

	// Findings travel in data, not in envelope.error. There is no single thing
	// that went wrong, so there is no one error to report — a caller reads the
	// findings array. The exit code still carries the outcome, which is what a
	// shell script and a CI gate branch on.
	if rep.HasErrors(doctorOpts.strict) {
		// The error is a summary so the envelope is never silently ok:true with
		// exit 4, which would break the one invariant the whole contract rests
		// on: a non-zero exit means the envelope said so.
		//
		// The exit code describes what the pass FOUND, not what it left behind.
		// A run that reported drift and then repaired it still exits 4, because
		// the findings it is carrying are real findings from a real state. The
		// alternative — re-checking after every fix and reporting the leftovers
		// as the findings — would mean a caller that reads `findings` and a
		// caller that reads the exit code are describing different moments. So
		// the hint says what to do next instead.
		res.Err = envelope.Errorf(
			envelope.CodeValidationFailed,
			doctorNextStep(layout.Root, len(rep.Fixed), doctorOpts.fixSafe),
			"%s", doctorSummary(rep, doctorOpts.strict),
		)
	}
	return res
}

func doctorNextStep(root string, fixed int, fixSafe bool) string {
	if fixSafe && fixed > 0 {
		return fmt.Sprintf("applied %d fix(es); run `ocaw --dir %s doctor` again to confirm", fixed, root)
	}
	return "ocaw --dir " + root + " doctor --fix-safe"
}

type doctorData struct {
	Findings  []doctor.Finding `json:"findings"`
	Fixed     []string         `json:"fixed"`
	Counts    map[string]int   `json:"counts"`
	Checks    []doctor.Check   `json:"checks"`
	VerifyCmd string           `json:"verify_cmd"`
	Strict    bool             `json:"strict"`
	FixSafe   bool             `json:"fix_safe"`
	DryRun    bool             `json:"dry_run"`
}

// ranChecks lists the checks that produced a finding, in run order, so a caller
// can ask "which parts of the workspace were looked at" without knowing the
// hardcoded list — a check that finds nothing is still a check that ran.
func ranChecks(rep *doctor.Report) []doctor.Check {
	seen := map[doctor.Check]bool{}
	var out []doctor.Check
	for _, check := range doctor.Checks {
		if seen[check] {
			continue
		}
		seen[check] = true
		if len(rep.Of(check)) > 0 {
			out = append(out, check)
		}
	}
	return out
}

func doctorSummary(rep *doctor.Report, strict bool) string {
	n := rep.FailingCount(strict)
	noun := "findings"
	if n == 1 {
		noun = "finding"
	}
	return fmt.Sprintf("%d failing %s (%d error, %d warning)", n, noun,
		rep.Counts()["error"], rep.Counts()["warning"])
}

func humanDoctor(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(doctorData)
	if !ok {
		return fmt.Errorf("doctor: unexpected data type %T", env.Data)
	}
	if len(data.Findings) == 0 {
		_, err := fmt.Fprintf(w, "no findings in %s\n", c.Dir)
		return err
	}
	for _, f := range data.Findings {
		if _, err := fmt.Fprintf(w, "%-7s %-14s %s\n", f.Severity, f.Code, f.Message); err != nil {
			return err
		}
		if f.Location != "" {
			if _, err := fmt.Fprintf(w, "        at %s\n", f.Location); err != nil {
				return err
			}
		}
		if f.Hint != "" {
			if _, err := fmt.Fprintf(w, "        try %s\n", f.Hint); err != nil {
				return err
			}
		}
	}
	for _, fixed := range data.Fixed {
		if _, err := fmt.Fprintf(w, "fixed:  %s\n", fixed); err != nil {
			return err
		}
	}
	counts := data.Counts
	_, err := fmt.Fprintf(w, "\n%d error, %d warning%s\n",
		counts["error"], counts["warning"], strictSuffix(data.Strict))
	return err
}

func strictSuffix(strict bool) string {
	if strict {
		return " (strict: warnings are counted as errors)"
	}
	return ""
}
