//go:build !linux

package evidence

// exchangeLatestPaths reports that atomic path exchange is unavailable on this platform.
func exchangeLatestPaths(string, string) (bool, error) {
	return false, nil
}
