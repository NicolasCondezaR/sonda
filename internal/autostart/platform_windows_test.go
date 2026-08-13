//go:build windows

package autostart

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScoopShimMustPointThroughCurrent(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "scoop", "shims")
	target := filepath.Join(root, "scoop", "apps", "sonda", "current", "sonda.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shimDir, "sonda.exe")
	if err := os.WriteFile(shim, []byte("shim"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "sonda.shim"), []byte(`path = "`+target+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyScoopShim(shim); err != nil {
		t.Fatalf("valid shim rejected: %v", err)
	}

	versioned := filepath.Join(root, "scoop", "apps", "sonda", "0.15.0", "sonda.exe")
	if err := os.WriteFile(filepath.Join(shimDir, "sonda.shim"), []byte(`path = "`+versioned+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyScoopShim(shim); err == nil || !strings.Contains(err.Error(), "apps/sonda/current") {
		t.Fatalf("versioned shim error = %v", err)
	}
}

func TestAStableShimIsDerivedFromAVersionedScoopExecutable(t *testing.T) {
	versioned := `C:\Users\Jane\scoop\apps\sonda\0.15.0\sonda.exe`
	want := `C:\Users\Jane\scoop\shims\sonda.exe`
	if got := scoopShimFor(versioned); !strings.EqualFold(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !isVersionedScoopPath(versioned) {
		t.Fatal("versioned Scoop path was not detected")
	}
}

func TestPortableExecutableWinsOverADifferentScoopInstallation(t *testing.T) {
	root := t.TempDir()
	portable := filepath.Join(root, ".sonda", "bin", "sonda.exe")
	oldTarget := filepath.Join(root, "scoop", "apps", "sonda", "current", "sonda.exe")
	shim := filepath.Join(root, "scoop", "shims", "sonda.exe")
	for _, path := range []string{portable, oldTarget, shim} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not an executable and must never be run by the resolver"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(strings.TrimSuffix(shim, filepath.Ext(shim))+".shim", []byte(`path = "`+oldTarget+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(shim)+string(os.PathListSeparator)+os.Getenv("PATH"))

	launcher, err := resolveLauncherFor(portable)
	if err != nil {
		t.Fatal(err)
	}
	if !launcher.Portable || !strings.EqualFold(launcher.Path, portable) {
		t.Fatalf("launcher=%+v, want portable current executable %q", launcher, portable)
	}
}

func TestCurrentScoopExecutableUsesOnlyItsEquivalentShim(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "scoop", "apps", "sonda", "current", "sonda.exe")
	shim := filepath.Join(root, "scoop", "shims", "sonda.exe")
	for _, path := range []string{current, shim} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		// Deliberately invalid bytes prove resolver compatibility checks never
		// execute the shim or target.
		if err := os.WriteFile(path, []byte("invalid executable bytes"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(strings.TrimSuffix(shim, filepath.Ext(shim))+".shim", []byte(`path = "`+current+`"`), 0o600); err != nil {
		t.Fatal(err)
	}

	launcher, err := resolveLauncherFor(current)
	if err != nil {
		t.Fatal(err)
	}
	if launcher.Portable || !strings.EqualFold(launcher.Path, shim) {
		t.Fatalf("launcher=%+v, want equivalent Scoop shim %q", launcher, shim)
	}
}

func TestCurrentScoopExecutableRejectsShimForAnotherTarget(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "scoop", "apps", "sonda", "current", "sonda.exe")
	other := filepath.Join(root, "other", "apps", "sonda", "current", "sonda.exe")
	shim := filepath.Join(root, "scoop", "shims", "sonda.exe")
	for _, path := range []string{current, other, shim} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(strings.TrimSuffix(shim, filepath.Ext(shim))+".shim", []byte(`path = "`+other+`"`), 0o600); err != nil {
		t.Fatal(err)
	}

	launcher, err := resolveLauncherFor(current)
	if err != nil {
		t.Fatal(err)
	}
	if !launcher.Portable || !strings.EqualFold(launcher.Path, current) {
		t.Fatalf("launcher=%+v, want current executable as portable", launcher)
	}
}

func TestUTF16TaskXMLRoundTrip(t *testing.T) {
	raw := []byte(xmlHeader + `<Task xmlns="` + taskNamespace + `"><RegistrationInfo><Description>Nicolás</Description></RegistrationInfo></Task>`)
	encoded := utf16XML(raw)
	decoded := decodeWindowsText(encoded)
	if !strings.Contains(string(decoded), "Nicolás") || !strings.Contains(string(decoded), `encoding="UTF-16"`) {
		t.Fatalf("decoded XML = %q", decoded)
	}
}

func TestGenerationLeaseProvesProcessLifetimeAndChangesPerStart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "sonda.yaml")
	control := windowsControl{}

	first, err := acquireGenerationLease(configPath)
	if err != nil {
		t.Fatal(err)
	}
	firstToken, active, err := control.Generation(configPath)
	if err != nil {
		first.close()
		t.Fatal(err)
	}
	if firstToken == "" || !active {
		first.close()
		t.Fatalf("first token=%q active=%t", firstToken, active)
	}
	if duplicate, err := acquireGenerationLease(configPath); err == nil {
		duplicate.close()
		first.close()
		t.Fatal("a second process acquired an active generation lease")
	}

	first.close()
	staleToken, active, err := control.Generation(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if staleToken != firstToken || active {
		t.Fatalf("released token=%q active=%t, want stale token %q and inactive", staleToken, active, firstToken)
	}

	second, err := acquireGenerationLease(configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	secondToken, active, err := control.Generation(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if secondToken == "" || secondToken == firstToken || !active {
		t.Fatalf("second token=%q active=%t, first=%q", secondToken, active, firstToken)
	}
}

func TestSchtasksRoundTripIntegration(t *testing.T) {
	if os.Getenv("SONDA_AUTOSTART_INTEGRATION") != "1" {
		t.Skip("set SONDA_AUTOSTART_INTEGRATION=1 to register one temporary current-user task")
	}
	if testing.Short() {
		t.Skip("external Task Scheduler integration")
	}

	sid, err := currentSID()
	if err != nil {
		t.Fatal(err)
	}
	name := "Sonda-Test-" + strings.ReplaceAll(time.Now().UTC().Format("150405.000"), ".", "")
	workingDirectory := t.TempDir()
	spec := TaskSpec{
		Name: name, SID: sid,
		Launcher:  filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"),
		Arguments: `/d /c exit 0`, WorkingDirectory: workingDirectory,
		Metadata: Metadata{
			Version: 1, ConfigPath: filepath.Join(workingDirectory, "sonda.yaml"),
			LogPath: filepath.Join(workingDirectory, "sonda.log"), ControlID: name,
			HealthURL: "http://127.0.0.1:1/health",
		},
	}
	raw, err := marshalTask(spec)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := &schtasksScheduler{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer scheduler.Delete(context.Background(), name)
	if err := scheduler.Register(ctx, name, raw); err != nil {
		t.Fatal(err)
	}
	queried, err := scheduler.Query(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseTask(queried); err != nil {
		t.Fatalf("Task Scheduler returned an unreadable definition: %v", err)
	}
	if err := scheduler.Start(ctx, name); err != nil {
		t.Fatal(err)
	}
}
