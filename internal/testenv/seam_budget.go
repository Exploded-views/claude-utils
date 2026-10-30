//go:build budget

package testenv

import "os"

// Fault injection for the budget gates' own tests: only in a -tags budget build,
// never in the product (testenv is imported by tests alone, and the product
// build has no seam at all).

func forceEndInvalid() bool  { return os.Getenv(EnvForceEndInvalid) == "1" }
func forceVerdictFail() bool { return os.Getenv(EnvForceVerdictFail) == "1" }
