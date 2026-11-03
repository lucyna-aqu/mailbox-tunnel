//go:build tools

package mobile

// gomobile bind needs golang.org/x/mobile/bind in this module's dependencies.
import _ "golang.org/x/mobile/bind"
