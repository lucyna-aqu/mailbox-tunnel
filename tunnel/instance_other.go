//go:build !windows

package tunnel

// AcquireSingleInstance always succeeds outside Windows.
func AcquireSingleInstance() bool {
	return true
}
