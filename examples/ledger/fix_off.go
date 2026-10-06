//go:build !ledgerfix

package main

// fixNullAmount is the planted bug's switch. The demo ships with the bug; build
// with -tags ledgerfix to get the fixed handler.
const fixNullAmount = false
