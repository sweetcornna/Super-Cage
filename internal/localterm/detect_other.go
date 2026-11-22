//go:build !darwin

package localterm

// Windows and Linux detection arrives with their tables (design step 5).
func applicationPath(string) string { return "" }
