package autostart

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
)

const (
	taskNamespace  = "http://schemas.microsoft.com/windows/2004/02/mit/task"
	metadataPrefix = "sonda-autostart-v1:"
)

// TaskSpec is the complete, user-scoped task Sonda owns. It deliberately has
// no password or elevated run level: the task only exists to bring back the
// same local process the user could start in a terminal.
type TaskSpec struct {
	Name             string
	SID              string
	Launcher         string
	Arguments        string
	WorkingDirectory string
	Metadata         Metadata
}

// Metadata is stored in RegistrationInfo.Documentation. Task Scheduler is
// free to normalise its XML, so comparing the facts we own is more reliable
// than comparing the bytes it exports with the bytes we imported.
type Metadata struct {
	Version          int    `json:"version"`
	ConfigPath       string `json:"config_path"`
	LogPath          string `json:"log_path"`
	ControlID        string `json:"control_id"`
	HealthURL        string `json:"health_url"`
	PortableLauncher bool   `json:"portable_launcher,omitempty"`
}

type taskDocument struct {
	XMLName          xml.Name         `xml:"Task"`
	XMLNS            string           `xml:"xmlns,attr,omitempty"`
	Version          string           `xml:"version,attr,omitempty"`
	RegistrationInfo registrationInfo `xml:"RegistrationInfo"`
	Triggers         triggers         `xml:"Triggers"`
	Principals       principals       `xml:"Principals"`
	Settings         taskSettings     `xml:"Settings"`
	Actions          actions          `xml:"Actions"`
}

type registrationInfo struct {
	Author        string `xml:"Author"`
	Description   string `xml:"Description"`
	URI           string `xml:"URI"`
	Documentation string `xml:"Documentation"`
}

type triggers struct {
	Logon logonTrigger `xml:"LogonTrigger"`
}

type logonTrigger struct {
	Enabled *bool  `xml:"Enabled"`
	UserID  string `xml:"UserId"`
}

type principals struct {
	Principal principal `xml:"Principal"`
}

type principal struct {
	ID        string `xml:"id,attr,omitempty"`
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type taskSettings struct {
	MultipleInstancesPolicy    string           `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool             `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool             `xml:"StopIfGoingOnBatteries"`
	AllowHardTerminate         *bool            `xml:"AllowHardTerminate"`
	StartWhenAvailable         bool             `xml:"StartWhenAvailable"`
	RunOnlyIfNetworkAvailable  bool             `xml:"RunOnlyIfNetworkAvailable"`
	IdleSettings               idleSettings     `xml:"IdleSettings"`
	AllowStartOnDemand         *bool            `xml:"AllowStartOnDemand"`
	Enabled                    *bool            `xml:"Enabled"`
	Hidden                     bool             `xml:"Hidden"`
	RunOnlyIfIdle              bool             `xml:"RunOnlyIfIdle"`
	WakeToRun                  bool             `xml:"WakeToRun"`
	ExecutionTimeLimit         string           `xml:"ExecutionTimeLimit"`
	Priority                   *int             `xml:"Priority"`
	RestartOnFailure           restartOnFailure `xml:"RestartOnFailure"`
}

type idleSettings struct {
	StopOnIdleEnd bool `xml:"StopOnIdleEnd"`
	RestartOnIdle bool `xml:"RestartOnIdle"`
}

type restartOnFailure struct {
	Interval string `xml:"Interval"`
	Count    int    `xml:"Count"`
}

type actions struct {
	Context string     `xml:"Context,attr,omitempty"`
	Exec    execAction `xml:"Exec"`
}

type execAction struct {
	Command          string `xml:"Command"`
	Arguments        string `xml:"Arguments"`
	WorkingDirectory string `xml:"WorkingDirectory"`
}

func taskNameForSID(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return fmt.Sprintf("Sonda-%x", sum[:6])
}

func controlID(taskName, configPath string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(taskName) + "\x00" + strings.ToLower(configPath)))
	return fmt.Sprintf("%s-%x", taskName, sum[:6])
}

func newTaskDocument(spec TaskSpec) (taskDocument, error) {
	metadata, err := encodeMetadata(spec.Metadata)
	if err != nil {
		return taskDocument{}, err
	}
	return taskDocument{
		XMLNS:   taskNamespace,
		Version: "1.4",
		RegistrationInfo: registrationInfo{
			Author:        "Sonda",
			Description:   "Starts Sonda for this user at logon. Managed by 'sonda autostart'.",
			URI:           `\` + spec.Name,
			Documentation: metadata,
		},
		Triggers: triggers{Logon: logonTrigger{
			Enabled: boolPointer(true),
			UserID:  spec.SID,
		}},
		Principals: principals{Principal: principal{
			ID:        "Author",
			UserID:    spec.SID,
			LogonType: "InteractiveToken",
			RunLevel:  "LeastPrivilege",
		}},
		Settings: taskSettings{
			MultipleInstancesPolicy:    "IgnoreNew",
			DisallowStartIfOnBatteries: false,
			StopIfGoingOnBatteries:     false,
			AllowHardTerminate:         boolPointer(true),
			StartWhenAvailable:         true,
			RunOnlyIfNetworkAvailable:  false,
			IdleSettings:               idleSettings{StopOnIdleEnd: false, RestartOnIdle: false},
			AllowStartOnDemand:         boolPointer(true),
			Enabled:                    boolPointer(true),
			Hidden:                     false,
			RunOnlyIfIdle:              false,
			WakeToRun:                  false,
			ExecutionTimeLimit:         "PT0S",
			Priority:                   intPointer(7),
			RestartOnFailure:           restartOnFailure{Interval: "PT1M", Count: 3},
		},
		Actions: actions{
			Context: "Author",
			Exec: execAction{
				Command:          spec.Launcher,
				Arguments:        spec.Arguments,
				WorkingDirectory: spec.WorkingDirectory,
			},
		},
	}, nil
}

func boolPointer(value bool) *bool { return &value }

func intPointer(value int) *int { return &value }

func marshalTask(spec TaskSpec) ([]byte, error) {
	doc, err := newTaskDocument(spec)
	if err != nil {
		return nil, err
	}
	raw, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode scheduled task: %w", err)
	}
	return append([]byte(xml.Header), raw...), nil
}

func parseTask(raw []byte) (taskDocument, Metadata, error) {
	var doc taskDocument
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return taskDocument{}, Metadata{}, fmt.Errorf("parse scheduled task: %w", err)
	}
	metadata, err := decodeMetadata(doc.RegistrationInfo.Documentation)
	if err != nil {
		return taskDocument{}, Metadata{}, err
	}
	return doc, metadata, nil
}

func encodeMetadata(metadata Metadata) (string, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode autostart metadata: %w", err)
	}
	return metadataPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMetadata(value string) (Metadata, error) {
	if !strings.HasPrefix(value, metadataPrefix) {
		return Metadata{}, fmt.Errorf("scheduled task is not managed by Sonda")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, metadataPrefix))
	if err != nil {
		return Metadata{}, fmt.Errorf("decode autostart metadata: %w", err)
	}
	var metadata Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode autostart metadata: %w", err)
	}
	if metadata.Version != 1 || metadata.ConfigPath == "" || metadata.ControlID == "" || metadata.HealthURL == "" {
		return Metadata{}, fmt.Errorf("scheduled task has incomplete Sonda metadata")
	}
	return metadata, nil
}

func taskMatches(spec TaskSpec, doc taskDocument, metadata Metadata) bool {
	return taskMismatch(spec, doc, metadata) == ""
}

func taskMismatch(spec TaskSpec, doc taskDocument, metadata Metadata) string {
	want, err := newTaskDocument(spec)
	if err != nil {
		return "definition"
	}
	checks := []struct {
		field string
		match bool
	}{
		{"xml.root", doc.XMLName.Local == "Task"},
		{"xml.namespace", doc.XMLName.Space == taskNamespace},
		{"xml.version", doc.Version == want.Version},
		{"registration.author", doc.RegistrationInfo.Author == want.RegistrationInfo.Author},
		{"registration.description", doc.RegistrationInfo.Description == want.RegistrationInfo.Description},
		{"registration.uri", doc.RegistrationInfo.URI == want.RegistrationInfo.URI},
		{"metadata.version", metadata.Version == spec.Metadata.Version},
		{"metadata.config_path", metadata.ConfigPath == spec.Metadata.ConfigPath},
		{"metadata.log_path", metadata.LogPath == spec.Metadata.LogPath},
		{"metadata.control_id", metadata.ControlID == spec.Metadata.ControlID},
		{"metadata.health_url", metadata.HealthURL == spec.Metadata.HealthURL},
		{"metadata.portable_launcher", metadata.PortableLauncher == spec.Metadata.PortableLauncher},
		{"registration.documentation", doc.RegistrationInfo.Documentation == want.RegistrationInfo.Documentation},
		{"trigger.enabled", optionalBoolIsTrue(doc.Triggers.Logon.Enabled)},
		{"trigger.user_id", doc.Triggers.Logon.UserID == want.Triggers.Logon.UserID},
		{"principal.id", doc.Principals.Principal.ID == want.Principals.Principal.ID},
		{"principal.user_id", doc.Principals.Principal.UserID == want.Principals.Principal.UserID},
		{"principal.logon_type", doc.Principals.Principal.LogonType == want.Principals.Principal.LogonType},
		{"principal.run_level", doc.Principals.Principal.RunLevel == "" || doc.Principals.Principal.RunLevel == want.Principals.Principal.RunLevel},
		{"settings.multiple_instances", doc.Settings.MultipleInstancesPolicy == want.Settings.MultipleInstancesPolicy},
		{"settings.disallow_start_on_batteries", doc.Settings.DisallowStartIfOnBatteries == want.Settings.DisallowStartIfOnBatteries},
		{"settings.stop_on_batteries", doc.Settings.StopIfGoingOnBatteries == want.Settings.StopIfGoingOnBatteries},
		{"settings.allow_hard_terminate", optionalBoolIsTrue(doc.Settings.AllowHardTerminate)},
		{"settings.start_when_available", doc.Settings.StartWhenAvailable == want.Settings.StartWhenAvailable},
		{"settings.network_required", doc.Settings.RunOnlyIfNetworkAvailable == want.Settings.RunOnlyIfNetworkAvailable},
		{"settings.idle.stop_on_end", doc.Settings.IdleSettings.StopOnIdleEnd == want.Settings.IdleSettings.StopOnIdleEnd},
		{"settings.idle.restart", doc.Settings.IdleSettings.RestartOnIdle == want.Settings.IdleSettings.RestartOnIdle},
		{"settings.allow_start_on_demand", optionalBoolIsTrue(doc.Settings.AllowStartOnDemand)},
		{"settings.enabled", optionalBoolIsTrue(doc.Settings.Enabled)},
		{"settings.hidden", doc.Settings.Hidden == want.Settings.Hidden},
		{"settings.run_only_if_idle", doc.Settings.RunOnlyIfIdle == want.Settings.RunOnlyIfIdle},
		{"settings.wake_to_run", doc.Settings.WakeToRun == want.Settings.WakeToRun},
		{"settings.execution_time_limit", doc.Settings.ExecutionTimeLimit == want.Settings.ExecutionTimeLimit},
		{"settings.priority", doc.Settings.Priority == nil || *doc.Settings.Priority == 7},
		{"settings.restart.interval", doc.Settings.RestartOnFailure.Interval == want.Settings.RestartOnFailure.Interval},
		{"settings.restart.count", doc.Settings.RestartOnFailure.Count == want.Settings.RestartOnFailure.Count},
		{"action.context", doc.Actions.Context == want.Actions.Context},
		{"action.command", doc.Actions.Exec.Command == want.Actions.Exec.Command},
		{"action.arguments", doc.Actions.Exec.Arguments == want.Actions.Exec.Arguments},
		{"action.working_directory", doc.Actions.Exec.WorkingDirectory == want.Actions.Exec.WorkingDirectory},
	}
	for _, check := range checks {
		if !check.match {
			return check.field
		}
	}
	return ""
}

func optionalBoolIsTrue(value *bool) bool { return value == nil || *value }
