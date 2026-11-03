//go:build !windows

package tunnel

import "errors"

var errNoSystemProxy = errors.New("setting the system proxy is only supported on Windows")

func ReadSystemProxy() (ProxySettings, error) {
	return ProxySettings{}, errNoSystemProxy
}

func WriteSystemProxy(ProxySettings) error {
	return errNoSystemProxy
}
