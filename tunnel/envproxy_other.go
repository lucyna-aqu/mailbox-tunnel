//go:build !windows

package tunnel

func ReadProxyEnvironment() (map[string]*string, error) {
	return nil, errNoSystemProxy
}

func WriteProxyEnvironment(map[string]*string) error {
	return errNoSystemProxy
}
