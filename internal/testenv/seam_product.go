//go:build !budget

package testenv

func forceEndInvalid() bool  { return false }
func forceVerdictFail() bool { return false }
