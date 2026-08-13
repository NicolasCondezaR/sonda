//go:build windows

package autostart

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// New returns the real Windows implementation. None of these dependencies
// mutate the machine until a lifecycle method is called.
func New() *Service {
	return newService(dependencies{
		scheduler:         &schtasksScheduler{},
		control:           windowsControl{},
		currentSID:        currentSID,
		resolveAccountSID: lookupAccountSID,
		resolveLauncher:   resolveLauncher,
		homeDir:           os.UserHomeDir,
	})
}

type schtasksScheduler struct{}

func (s *schtasksScheduler) Query(ctx context.Context, taskName string) ([]byte, error) {
	out, err := runSchtasks(ctx, "/Query", "/TN", taskName, "/XML")
	if err == nil {
		return normaliseTaskXML(out), nil
	}
	taskFile := filepath.Join(os.Getenv("SystemRoot"), "System32", "Tasks", taskName)
	if _, statErr := os.Stat(taskFile); errors.Is(statErr, os.ErrNotExist) {
		return nil, ErrNotInstalled
	}
	return nil, err
}

func (s *schtasksScheduler) Register(ctx context.Context, taskName string, raw []byte) error {
	temp, err := os.CreateTemp("", "sonda-task-*.xml")
	if err != nil {
		return err
	}
	path := temp.Name()
	defer os.Remove(path)

	encoded := utf16XML(raw)
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// Register is create-only. Service.Install already refuses an occupied name,
	// and omitting /F keeps a task created in the query/create race from being
	// overwritten by Task Scheduler itself.
	_, err = runSchtasks(ctx, "/Create", "/TN", taskName, "/XML", path)
	return err
}

func (s *schtasksScheduler) Start(ctx context.Context, taskName string) error {
	_, err := runSchtasks(ctx, "/Run", "/TN", taskName)
	return err
}

func (s *schtasksScheduler) End(ctx context.Context, taskName string) error {
	_, err := runSchtasks(ctx, "/End", "/TN", taskName)
	return err
}

func (s *schtasksScheduler) Delete(ctx context.Context, taskName string) error {
	_, err := runSchtasks(ctx, "/Delete", "/TN", taskName, "/F")
	return err
}

func runSchtasks(ctx context.Context, argv ...string) ([]byte, error) {
	path := filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe")
	if os.Getenv("SystemRoot") == "" {
		path = "schtasks.exe"
	}
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	decoded := decodeWindowsText(out)
	if err != nil {
		return nil, fmt.Errorf("schtasks %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(decoded)))
	}
	return decoded, nil
}

func utf16XML(raw []byte) []byte {
	text := strings.TrimPrefix(string(raw), xmlHeader)
	text = `<?xml version="1.0" encoding="UTF-16"?>` + "\r\n" + text
	encoded := utf16.Encode([]rune(text))
	out := make([]byte, 2+len(encoded)*2)
	out[0], out[1] = 0xff, 0xfe
	for i, value := range encoded {
		binary.LittleEndian.PutUint16(out[2+i*2:], value)
	}
	return out
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

func decodeWindowsText(raw []byte) []byte {
	if len(raw) < 2 {
		return raw
	}
	if raw[0] == 0xff && raw[1] == 0xfe {
		values := make([]uint16, (len(raw)-2)/2)
		for i := range values {
			values[i] = binary.LittleEndian.Uint16(raw[2+i*2:])
		}
		return []byte(string(utf16.Decode(values)))
	}
	// Schtasks sometimes omits the BOM when stdout is redirected. NUL bytes
	// in every other position still identify UTF-16LE unambiguously.
	if len(raw) >= 4 && raw[1] == 0 && raw[3] == 0 {
		values := make([]uint16, len(raw)/2)
		for i := range values {
			values[i] = binary.LittleEndian.Uint16(raw[i*2:])
		}
		return []byte(string(utf16.Decode(values)))
	}
	return raw
}

func normaliseTaskXML(raw []byte) []byte {
	text := string(raw)
	text = strings.Replace(text, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	text = strings.Replace(text, `encoding='UTF-16'`, `encoding='UTF-8'`, 1)
	return []byte(text)
}

func currentSID() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func lookupAccountSID(account string) (string, error) {
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

type windowsControl struct{}

func (windowsControl) Probe(controlID string) bool {
	name, err := windows.UTF16PtrFromString(eventName(controlID))
	if err != nil {
		return false
	}
	handle, err := windows.OpenEvent(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false
	}
	windows.CloseHandle(handle)
	return true
}

func (windowsControl) Signal(controlID string) error {
	name, err := windows.UTF16PtrFromString(eventName(controlID))
	if err != nil {
		return err
	}
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return ErrNotRunning
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.SetEvent(handle)
}

func (windowsControl) Generation(configPath string) (string, bool, error) {
	path := generationPath(configPath)
	token, err := readGenerationToken(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return token, true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("probe generation marker %s: %w", path, err)
	}
	windows.CloseHandle(handle)
	return token, false, nil
}

func eventName(controlID string) string { return `Local\Sonda-` + controlID }

// ListenForStop creates the current-user event used by `autostart stop` and a
// per-config generation lease. The lease denies writers until Windows closes
// the handle at process termination, which lets restart distinguish a fully
// exited process from one that merely closed its health listener and event.
// The default DACL comes from the current token; no global or unauthenticated
// IPC endpoint is opened.
func ListenForStop(controlID, configPath string) (<-chan struct{}, func(), error) {
	if controlID == "" {
		return nil, func() {}, nil
	}
	lease, err := acquireGenerationLease(configPath)
	if err != nil {
		return nil, nil, err
	}
	name, err := windows.UTF16PtrFromString(eventName(controlID))
	if err != nil {
		lease.close()
		return nil, nil, err
	}
	handle, err := windows.CreateEvent(nil, 1, 0, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(handle)
		lease.close()
		return nil, nil, fmt.Errorf("another Sonda process already owns autostart control %s", controlID)
	}
	if err != nil {
		lease.close()
		return nil, nil, err
	}
	stopped := make(chan struct{})
	go func() {
		result, _ := windows.WaitForSingleObject(handle, windows.INFINITE)
		if result == windows.WAIT_OBJECT_0 {
			close(stopped)
		}
	}()
	// Deliberately do not close lease here. The operating system closes it only
	// once the process has actually terminated, after every Go defer completed.
	return stopped, func() { windows.CloseHandle(handle) }, nil
}

type generationLease struct {
	handle windows.Handle
}

func acquireGenerationLease(configPath string) (*generationLease, error) {
	path := generationPath(configPath)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, fmt.Errorf("another Sonda process generation still owns %s", path)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire autostart generation marker %s: %w", path, err)
	}
	lease := &generationLease{handle: handle}
	fail := func(operation string, cause error) (*generationLease, error) {
		lease.close()
		return nil, fmt.Errorf("%s autostart generation marker %s: %w", operation, path, cause)
	}

	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return fail("generate", err)
	}
	token := []byte(hex.EncodeToString(bytes))
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return fail("seek", err)
	}
	if err := windows.SetEndOfFile(handle); err != nil {
		return fail("truncate", err)
	}
	var written uint32
	if err := windows.WriteFile(handle, token, &written, nil); err != nil {
		return fail("write", err)
	}
	if written != uint32(len(token)) {
		return fail("write", io.ErrShortWrite)
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fail("flush", err)
	}
	return lease, nil
}

func (l *generationLease) close() {
	if l == nil || l.handle == 0 {
		return
	}
	windows.CloseHandle(l.handle)
	l.handle = 0
}

func readGenerationToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", fmt.Errorf("read generation marker %s: %w", path, err)
	}
	if len(raw) > 128 {
		return "", fmt.Errorf("generation marker %s exceeds 128 bytes", path)
	}
	return strings.TrimSpace(string(raw)), nil
}

var scoopShimPath = regexp.MustCompile(`(?mi)^\s*path\s*=\s*"([^"]+)"\s*$`)

func resolveLauncher() (Launcher, error) {
	executable, err := os.Executable()
	if err != nil {
		return Launcher{}, fmt.Errorf("find the running sonda.exe: %w", err)
	}
	return resolveLauncherFor(executable)
}

func resolveLauncherFor(executable string) (Launcher, error) {
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return Launcher{}, fmt.Errorf("resolve the running sonda.exe: %w", err)
	}
	if _, err := os.Stat(absolute); err != nil {
		return Launcher{}, fmt.Errorf("running executable %s is unavailable: %w", absolute, err)
	}

	if isScoopCurrentPath(absolute) {
		shim := scoopShimFor(absolute)
		target, err := scoopShimTarget(shim)
		if err == nil && samePath(target, absolute) {
			return Launcher{Path: shim}, nil
		}
	}

	// The running executable is the only safe fallback. In particular, never
	// select or execute a different sonda.exe merely because PATH or Scoop has
	// an older installation.
	return Launcher{
		Path: absolute, Portable: true,
		Warning: "the task points to a portable/local executable; place it at its final path before installing autostart",
	}, nil
}

func isScoopShim(path string) bool {
	dir := strings.ToLower(filepath.ToSlash(filepath.Dir(path)))
	return strings.HasSuffix(dir, "/scoop/shims")
}

func isVersionedScoopPath(path string) bool {
	parts := strings.Split(strings.ToLower(filepath.ToSlash(path)), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "apps" && parts[i+1] == "sonda" {
			return parts[i+2] != "current"
		}
	}
	return false
}

func isScoopCurrentPath(path string) bool {
	return strings.Contains(strings.ToLower(filepath.ToSlash(path)), "/apps/sonda/current/")
}

func scoopShimFor(path string) string {
	clean := filepath.Clean(path)
	lower := strings.ToLower(filepath.ToSlash(clean))
	const marker = "/apps/sonda/"
	index := strings.Index(lower, marker)
	if index < 0 {
		return ""
	}
	root := filepath.FromSlash(filepath.ToSlash(clean)[:index])
	return filepath.Join(root, "shims", "sonda.exe")
}

func verifyScoopShim(path string) error {
	_, err := scoopShimTarget(path)
	return err
}

func scoopShimTarget(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("Scoop shim %s is unavailable: %w", path, err)
	}
	metadata := strings.TrimSuffix(path, filepath.Ext(path)) + ".shim"
	raw, err := os.ReadFile(metadata)
	if err != nil {
		return "", fmt.Errorf("Scoop shim %s cannot be verified: %w", path, err)
	}
	match := scoopShimPath.FindSubmatch(raw)
	if len(match) != 2 {
		return "", fmt.Errorf("Scoop shim %s has no verifiable target", path)
	}
	target := string(match[1])
	if !filepath.IsAbs(target) || !strings.Contains(strings.ToLower(filepath.ToSlash(target)), "/apps/sonda/current/") {
		return "", fmt.Errorf("Scoop shim %s does not point through apps/sonda/current", path)
	}
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("Scoop shim target %s is unavailable: %w", target, err)
	}
	return filepath.Clean(target), nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
