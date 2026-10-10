//go:build ledgerpartialfix

package main

// partialFix is a plausible wrong fix for the planted bug: it guards only the
// event type seen in the incident. Build with -tags ledgerpartialfix to watch
// kavach diff reject it on a variant of the incident.
const partialFix = true
