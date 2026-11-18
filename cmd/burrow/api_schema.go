package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/getkiko/burrow/diag"
	"github.com/getkiko/burrow/internal/birthdaylab"
	"github.com/urfave/cli/v2"
)

type apiCatalog struct {
	Tabs []apiTabSpec `json:"tabs"`
}

type apiTabSpec struct {
	ID       string           `json:"id"`
	Title    string           `json:"title"`
	Commands []apiCommandSpec `json:"commands"`
}

type apiCommandSpec struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Role     string         `json:"role,omitempty"`
	Command  string         `json:"command"`
	Endpoint string         `json:"endpoint"`
	Path     []string       `json:"-"`
	Fields   []apiFieldSpec `json:"fields"`
}

type apiFieldSpec struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Input       string   `json:"input"`
	Required    bool     `json:"required"`
	Default     any      `json:"default"`
	Minimum     *int64   `json:"minimum,omitempty"`
	Maximum     *int64   `json:"maximum,omitempty"`
	Options     []string `json:"options,omitempty"`
}

func newAPICatalog() apiCatalog {
	tcpNAT := commandToAPISpec(newTCPNATCommand(), nil, "/api/tcpnat", "TCP NAT", "")
	stun := commandToAPISpec(newSTUNCommand(), nil, "/api/stun", "NAT Type", "")
	punchCommand := newPunchCommand()
	birthdayCommand := newBirthdayCommand()
	return apiCatalog{Tabs: []apiTabSpec{
		{ID: "tcpnat", Title: "TCP NAT", Commands: []apiCommandSpec{tcpNAT}},
		{ID: "stun", Title: "STUN", Commands: []apiCommandSpec{stun}},
		{ID: "punch", Title: "Punch", Commands: []apiCommandSpec{
			commandToAPISpec(findSubcommand(punchCommand, "client"), []string{"punch"}, "/api/punch/client", "Punch Client", "client"),
			commandToAPISpec(findSubcommand(punchCommand, "server"), []string{"punch"}, "/api/punch/server", "Punch Server", "server"),
		}},
		{ID: "birthday", Title: "Birthday", Commands: []apiCommandSpec{
			commandToAPISpec(findSubcommand(birthdayCommand, "client"), []string{"birthday"}, "/api/birthday/client", "Birthday Client", "client"),
			commandToAPISpec(findSubcommand(birthdayCommand, "server"), []string{"birthday"}, "/api/birthday/server", "Birthday Server", "server"),
		}},
	}}
}

func findSubcommand(parent *cli.Command, name string) *cli.Command {
	for _, command := range parent.Subcommands {
		if command.Name == name {
			return command
		}
	}
	panic("subcommand missing: " + parent.Name + " " + name)
}

func commandToAPISpec(command *cli.Command, parent []string, endpoint, title, role string) apiCommandSpec {
	path := append(append([]string{}, parent...), command.Name)
	fields := make([]apiFieldSpec, 0, len(command.Flags))
	for _, flag := range command.Flags {
		if !includeFlagInAPI(path, flag) {
			continue
		}
		fields = append(fields, flagToAPIField(flag))
	}
	spec := apiCommandSpec{
		ID:       strings.Join(path, "/"),
		Title:    title,
		Role:     role,
		Command:  strings.Join(path, " "),
		Endpoint: endpoint,
		Path:     path,
		Fields:   fields,
	}
	if spec.ID == "birthday/server" {
		for index := range spec.Fields {
			switch spec.Fields[index].Name {
			case "signal":
				spec.Fields[index].Label = "Signal servers"
				spec.Fields[index].Description = "The first line is used for client pairing. In NAT mode, add more servers on different public IPs to verify that the mapping is stable; in fixed mode, enter one line."
				spec.Fields[index].Input = "textarea"
				spec.Fields[index].Required = true
			case "mode":
				spec.Fields[index].Description = "Use nat behind a home or office NAT; use fixed on a public host or behind a port forward."
				spec.Fields[index].Default = "nat"
				spec.Fields[index].Options = []string{"nat", "fixed"}
			}
		}
	}
	return spec
}

func includeFlagInAPI(path []string, flag cli.Flag) bool {
	commandID := strings.Join(path, "/")
	for _, name := range flag.Names() {
		if name == "signal_path" || name == "signal-path" || name == "probe-ports" {
			return false
		}
		if commandID == "birthday/server" {
			switch name {
			case "probe-signals", "trials":
				return false
			}
		}
	}
	return true
}

func flagToAPIField(flag cli.Flag) apiFieldSpec {
	field := apiFieldSpec{Input: "text"}
	switch value := flag.(type) {
	case *cli.StringFlag:
		field.Name = value.Name
		field.Description = value.Usage
		field.Type = "string"
		field.Required = value.Required
		field.Default = value.Value
	case *cli.IntFlag:
		field.Name = value.Name
		field.Description = value.Usage
		field.Type = "integer"
		field.Required = value.Required
		field.Default = value.Value
		field.Input = "number"
	case *cli.DurationFlag:
		field.Name = value.Name
		field.Description = value.Usage
		field.Type = "duration"
		field.Required = value.Required
		field.Default = value.Value.String()
	case *cli.BoolFlag:
		field.Name = value.Name
		field.Description = value.Usage
		field.Type = "boolean"
		field.Required = value.Required
		field.Default = value.Value
		field.Input = "toggle"
	default:
		panic(fmt.Sprintf("unsupported CLI flag %T", flag))
	}
	field.Label = apiFieldLabel(field.Name)
	annotateAPIField(&field)
	return field
}

func apiFieldLabel(name string) string {
	labels := map[string]string{
		"signal":          "Signal address",
		"signal_path":     "Signal path",
		"server":          "Signal server",
		"ports":           "Probe ports",
		"probe-ports":     "Signal probe ports",
		"probe-signals":   "Signal probe servers",
		"room":            "Room",
		"iface":           "Interface",
		"local-port":      "Local port",
		"rounds":          "Rounds",
		"timeout":         "Timeout",
		"msg":             "Message",
		"stun":            "STUN server",
		"bench":           "Throughput test",
		"size":            "Transfer size",
		"mode":            "Server network",
		"public-address":  "Public address",
		"candidates":      "Candidates",
		"trials":          "Trials",
		"min-port":        "Min port",
		"max-port":        "Max port",
		"warmup":          "Warmup",
		"attempt-timeout": "Attempt timeout",
		"pair-timeout":    "Pairing timeout",
	}
	if label := labels[name]; label != "" {
		return label
	}
	return name
}

func annotateAPIField(field *apiFieldSpec) {
	minimums := map[string]int64{
		"local-port": 0,
		"rounds":     1,
		"candidates": 1,
		"trials":     1,
		"min-port":   1,
		"max-port":   1,
	}
	if minimum, exists := minimums[field.Name]; exists {
		field.Minimum = &minimum
	}
	maximums := map[string]int64{
		"local-port": 65535,
		"candidates": birthdaylab.MaxCandidates,
		"min-port":   65535,
		"max-port":   65535,
	}
	if maximum, exists := maximums[field.Name]; exists {
		field.Maximum = &maximum
	}
	switch field.Name {
	case "mode":
		field.Input = "select"
		field.Options = []string{"auto", "nat", "fixed"}
	case "msg", "probe-signals":
		field.Input = "textarea"
	}
}

func normalizeAPICommandValues(spec apiCommandSpec, raw map[string]any) (map[string]any, error) {
	fields := make(map[string]apiFieldSpec, len(spec.Fields))
	normalized := make(map[string]any, len(spec.Fields))
	for _, field := range spec.Fields {
		fields[field.Name] = field
		normalized[field.Name] = field.Default
	}
	for name := range raw {
		if _, exists := fields[name]; !exists {
			return nil, fmt.Errorf("unknown argument %q", name)
		}
	}

	for _, field := range spec.Fields {
		value, exists := raw[field.Name]
		if !exists {
			if field.Required && isEmptyAPIValue(field.Default) {
				return nil, fmt.Errorf("argument %s is required", field.Name)
			}
			continue
		}
		parsed, err := parseAPIFieldValue(field, value)
		if err != nil {
			return nil, err
		}
		if field.Required && isEmptyAPIValue(parsed) {
			return nil, fmt.Errorf("argument %s is required", field.Name)
		}
		normalized[field.Name] = parsed
	}
	if err := validateAPICommand(spec, normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func parseAPIFieldValue(field apiFieldSpec, value any) (any, error) {
	switch field.Type {
	case "string":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("argument %s must be a string", field.Name)
		}
		if len(field.Options) > 0 && !containsString(field.Options, text) {
			return nil, fmt.Errorf("argument %s must be one of %s", field.Name, strings.Join(field.Options, ", "))
		}
		return text, nil
	case "integer":
		var number int64
		switch typed := value.(type) {
		case json.Number:
			parsed, err := typed.Int64()
			if err != nil {
				return nil, fmt.Errorf("argument %s must be an integer", field.Name)
			}
			number = parsed
		case float64:
			if math.Trunc(typed) != typed || typed < math.MinInt64 || typed > math.MaxInt64 {
				return nil, fmt.Errorf("argument %s must be an integer", field.Name)
			}
			number = int64(typed)
		case string:
			parsed, err := strconv.ParseInt(typed, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("argument %s must be an integer", field.Name)
			}
			number = parsed
		default:
			return nil, fmt.Errorf("argument %s must be an integer", field.Name)
		}
		if field.Minimum != nil && number < *field.Minimum {
			return nil, fmt.Errorf("argument %s must be at least %d", field.Name, *field.Minimum)
		}
		if field.Maximum != nil && number > *field.Maximum {
			return nil, fmt.Errorf("argument %s must be at most %d", field.Name, *field.Maximum)
		}
		if number > math.MaxInt {
			return nil, fmt.Errorf("argument %s is out of range", field.Name)
		}
		return int(number), nil
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("argument %s must be a boolean", field.Name)
		}
		return boolean, nil
	case "duration":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("argument %s must be a duration string", field.Name)
		}
		duration, err := time.ParseDuration(text)
		if err != nil || duration <= 0 {
			return nil, fmt.Errorf("argument %s must be a positive duration such as 3s or 250ms", field.Name)
		}
		return text, nil
	default:
		return nil, fmt.Errorf("argument %s has an invalid type", field.Name)
	}
}

func validateAPICommand(spec apiCommandSpec, values map[string]any) error {
	if strings.TrimSpace(stringValue(values, "signal")) == "" && strings.HasPrefix(spec.ID, "punch/") {
		return fmt.Errorf("argument signal is required")
	}
	switch spec.ID {
	case "tcpnat":
		if strings.TrimSpace(stringValue(values, "server")) == "" {
			return fmt.Errorf("argument server is required")
		}
		if _, err := diag.ParsePorts(stringValue(values, "ports")); err != nil {
			return err
		}
	case "stun":
		if _, err := diag.NormalizeSTUNServer(stringValue(values, "stun")); err != nil {
			return err
		}
	case "punch/client":
		if boolValue(values, "bench") {
			if _, err := parseSize(stringValue(values, "size")); err != nil {
				return fmt.Errorf("invalid argument size: %w", err)
			}
		}
	case "birthday/client":
		candidates := intValue(values, "candidates")
		trials := intValue(values, "trials")
		minPort := intValue(values, "min-port")
		maxPort := intValue(values, "max-port")
		if _, err := validateExperiment(candidates, trials, minPort, maxPort); err != nil {
			return err
		}
		if minPort < 1 || maxPort > 65535 {
			return fmt.Errorf("port range must be within 1-65535")
		}
		if _, err := birthdaylab.NewTrialParameters(
			candidates,
			minPort,
			maxPort,
			durationValue(values, "warmup"),
			durationValue(values, "attempt-timeout"),
		); err != nil {
			return err
		}
	case "birthday/server":
		if _, err := birthdaylab.ParseSignalAddresses(stringValue(values, "signal")); err != nil {
			return err
		}
	}
	if spec.ID == "birthday/server" && stringValue(values, "mode") == "fixed" {
		if intValue(values, "local-port") <= 0 || strings.TrimSpace(stringValue(values, "public-address")) == "" {
			return fmt.Errorf("fixed mode requires local-port and public-address")
		}
	}
	return nil
}

func isEmptyAPIValue(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) == ""
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func stringValue(values map[string]any, name string) string {
	value, _ := values[name].(string)
	return value
}

func intValue(values map[string]any, name string) int {
	value, _ := values[name].(int)
	return value
}

func boolValue(values map[string]any, name string) bool {
	value, _ := values[name].(bool)
	return value
}
