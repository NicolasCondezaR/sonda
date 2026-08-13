package autostart

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestTaskDefinitionCarriesTheRequiredSafetyContract(t *testing.T) {
	spec := TaskSpec{
		Name:             "Sonda-123456789abc",
		SID:              "S-1-5-21-1000",
		Launcher:         `C:\Users\Jane Doe\scoop\shims\sonda.exe`,
		Arguments:        `-config "C:\Users\Jane Doe\.sonda\sonda.yaml"`,
		WorkingDirectory: `C:\Users\Jane Doe\.sonda`,
		Metadata: Metadata{
			Version: 1, ConfigPath: `C:\Users\Jane Doe\.sonda\sonda.yaml`,
			LogPath: `C:\Users\Jane Doe\.sonda\sonda.log`, ControlID: "control",
			HealthURL: "http://127.0.0.1:9000/health",
		},
	}
	raw, err := marshalTask(spec)
	if err != nil {
		t.Fatal(err)
	}
	doc, metadata, err := parseTask(raw)
	if err != nil {
		t.Fatal(err)
	}

	if metadata != spec.Metadata {
		t.Fatalf("metadata = %+v, want %+v", metadata, spec.Metadata)
	}
	if !taskMatches(spec, doc, metadata) {
		t.Fatal("round-tripped task no longer matches its managed definition")
	}
	if doc.Triggers.Logon.UserID != spec.SID || doc.Triggers.Logon.Enabled == nil || !*doc.Triggers.Logon.Enabled {
		t.Errorf("logon trigger = %+v", doc.Triggers.Logon)
	}
	principal := doc.Principals.Principal
	if principal.UserID != spec.SID || principal.LogonType != "InteractiveToken" || principal.RunLevel != "LeastPrivilege" {
		t.Errorf("principal = %+v", principal)
	}
	settings := doc.Settings
	if settings.MultipleInstancesPolicy != "IgnoreNew" || settings.DisallowStartIfOnBatteries || settings.StopIfGoingOnBatteries {
		t.Errorf("instance/battery settings = %+v", settings)
	}
	if !settings.StartWhenAvailable || settings.ExecutionTimeLimit != "PT0S" {
		t.Errorf("availability/time limit = %+v", settings)
	}
	if settings.RestartOnFailure.Count != 3 || settings.RestartOnFailure.Interval != "PT1M" {
		t.Errorf("restart policy = %+v", settings.RestartOnFailure)
	}
}

func TestTaskXMLAndArgumentsEscapePathsWithoutChangingThem(t *testing.T) {
	paths := []string{
		`C:\Users\Jane Doe\.sonda\sonda.yaml`,
		`C:\Users\Ampersand & Sons\<sonda>\sonda.yaml`,
		`C:\Users\Nicolás\quote"dir\sonda.yaml`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			spec := TaskSpec{
				Name: "Sonda-test", SID: "S-1-5-21-1000",
				Launcher:         `C:\Program Files\Sonda & Tools\sonda.exe`,
				Arguments:        windowsCommandLine([]string{"-config", path}),
				WorkingDirectory: `C:\Users\Jane Doe\.sonda`,
				Metadata:         Metadata{Version: 1, ConfigPath: path, LogPath: path + ".log", ControlID: "control", HealthURL: "http://127.0.0.1:9000/health"},
			}
			raw, err := marshalTask(spec)
			if err != nil {
				t.Fatal(err)
			}
			var valid struct{ XMLName xml.Name }
			if err := xml.Unmarshal(raw, &valid); err != nil {
				t.Fatalf("invalid XML: %v\n%s", err, raw)
			}
			doc, metadata, err := parseTask(raw)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Actions.Exec.Command != spec.Launcher || doc.Actions.Exec.Arguments != spec.Arguments || metadata.ConfigPath != path {
				t.Fatalf("round trip changed values: action=%+v metadata=%+v", doc.Actions.Exec, metadata)
			}
		})
	}
}

func TestWindowsArgumentQuoting(t *testing.T) {
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "plain", arg: "-config", want: "-config"},
		{name: "space", arg: `C:\Jane Doe\sonda.yaml`, want: `"C:\Jane Doe\sonda.yaml"`},
		{name: "empty", arg: "", want: `""`},
		{name: "quote", arg: `a"b`, want: `"a\"b"`},
		{name: "trailing slash", arg: `C:\Jane Doe\`, want: `"C:\Jane Doe\\"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quoteWindowsArg(tt.arg); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTaskIdentityIsStableAndUserScoped(t *testing.T) {
	a := taskNameForSID("S-1-5-21-1000")
	b := taskNameForSID("S-1-5-21-1000")
	c := taskNameForSID("S-1-5-21-2000")
	if a != b || a == c || !strings.HasPrefix(a, "Sonda-") {
		t.Fatalf("task names = %q %q %q", a, b, c)
	}
}

func TestCanonicalDefaultsAcceptElisionButRejectExplicitUnsafeValues(t *testing.T) {
	spec := TaskSpec{
		Name: "Sonda-test", SID: "S-1-5-21-1000",
		Launcher: `C:\sonda.exe`, Arguments: `-config C:\sonda.yaml`, WorkingDirectory: `C:\`,
		Metadata: Metadata{Version: 1, ConfigPath: `C:\sonda.yaml`, LogPath: `C:\sonda.log`, ControlID: "control", HealthURL: "http://127.0.0.1:9000/health"},
	}
	raw, err := marshalTask(spec)
	if err != nil {
		t.Fatal(err)
	}
	doc, metadata, err := parseTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc.Triggers.Logon.Enabled = nil
	doc.Principals.Principal.RunLevel = ""
	doc.Settings.AllowHardTerminate = nil
	doc.Settings.AllowStartOnDemand = nil
	doc.Settings.Enabled = nil
	doc.Settings.Priority = nil
	if mismatch := taskMismatch(spec, doc, metadata); mismatch != "" {
		t.Fatalf("default elision mismatch=%s", mismatch)
	}

	tests := []struct {
		name  string
		field string
		apply func(*taskDocument)
	}{
		{name: "disabled trigger", field: "trigger.enabled", apply: func(doc *taskDocument) { doc.Triggers.Logon.Enabled = boolPointer(false) }},
		{name: "elevated principal", field: "principal.run_level", apply: func(doc *taskDocument) { doc.Principals.Principal.RunLevel = "HighestAvailable" }},
		{name: "hard termination disabled", field: "settings.allow_hard_terminate", apply: func(doc *taskDocument) { doc.Settings.AllowHardTerminate = boolPointer(false) }},
		{name: "demand start disabled", field: "settings.allow_start_on_demand", apply: func(doc *taskDocument) { doc.Settings.AllowStartOnDemand = boolPointer(false) }},
		{name: "task disabled", field: "settings.enabled", apply: func(doc *taskDocument) { doc.Settings.Enabled = boolPointer(false) }},
		{name: "priority changed", field: "settings.priority", apply: func(doc *taskDocument) { doc.Settings.Priority = intPointer(1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := doc
			tt.apply(&mutated)
			if mismatch := taskMismatch(spec, mutated, metadata); mismatch != tt.field {
				t.Fatalf("mismatch=%q, want %q", mismatch, tt.field)
			}
		})
	}
}
