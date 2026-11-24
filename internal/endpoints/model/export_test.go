package model

// SwapOwnerCheck installs f as the constructor of the owner check each new
// call claims (end.go) and returns the one it replaced, for a test that
// watches which calls the handler claims.
func SwapOwnerCheck(f func() func()) (previous func() func()) {
	previous, newOwnerCheck = newOwnerCheck, f
	return previous
}
