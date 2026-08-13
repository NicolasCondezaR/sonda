//go:build !windows

package autostart

// New returns a clear unsupported service on non-Windows platforms. Sonda's
// ordinary foreground process and MCP adapter remain fully portable.
func New() *Service { return newService(dependencies{}) }

// ListenForStop is only meaningful for a Windows Scheduled Task.
func ListenForStop(controlID, _ string) (<-chan struct{}, func(), error) {
	if controlID == "" {
		return nil, func() {}, nil
	}
	return nil, func() {}, ErrUnsupported
}
