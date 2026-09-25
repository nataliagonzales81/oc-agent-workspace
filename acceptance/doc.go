// Package acceptance holds the end-to-end suite for SPEC §10.
//
// It is deliberately outside internal/. The acceptance tests drive the built CLI
// the way an agent does — argv in, envelope out, exit code — and they are
// allowed to shell out to the Go toolchain for the static build check, which
// internal/{state,report,workspace,yaml,envelope,cli} is not. The rule in
// internal/cli/status_test.go is about the shipped tool not spawning processes;
// a test suite is not the shipped tool, and folding the two together would mean
// weakening either one.
//
// The tests read their assertions out of the decoded envelope with map keys
// rather than against internal types. That is the point: the suite is written
// the way a consumer reads the contract, so a payload that stopped being the
// documented shape fails here rather than in somebody's agent.
package acceptance
