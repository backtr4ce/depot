package main

import (
	"fmt"
	"strings"
)

// Built-in variables. host_* describe the machine running depot and are
// read-only; target_* default to the host and may be overridden with --set.
const (
	varHostOS    = "host_os"
	varHostCPU   = "host_cpu"
	varTargetOS  = "target_os"
	varTargetCPU = "target_cpu"

	hostPrefix   = "host_"
	targetPrefix = "target_"
)

// keywords have a meaning in conditions and cannot name variables.
var keywords = map[string]bool{
	"true":  true,
	"false": true,
	"and":   true,
	"or":    true,
	"not":   true,
}

func isIdentStart(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || ('0' <= c && c <= '9')
}

// isIdentifier reports whether name is an ASCII identifier such as
// use_old_tool or target_cpu.
func isIdentifier(name string) bool {
	if name == "" || !isIdentStart(name[0]) {
		return false
	}

	for i := 1; i < len(name); i++ {
		if !isIdentPart(name[i]) {
			return false
		}
	}

	return true
}

// checkVariableName validates the syntax shared by --set and vars.
func checkVariableName(name string) error {
	if !isIdentifier(name) || keywords[name] {
		return fmt.Errorf("invalid variable name %q", name)
	}

	return nil
}

// checkOverride validates a --set name. Only host_* is immutable.
func checkOverride(name string) error {
	if err := checkVariableName(name); err != nil {
		return err
	}

	if strings.HasPrefix(name, hostPrefix) {
		return fmt.Errorf("%s is reserved for built-in variables and read-only", name)
	}

	return nil
}

// checkDeclaration validates a name declared in a manifest's vars block.
// Besides the read-only host_*, target_* is reserved because it already
// defaults to the host.
func checkDeclaration(name string) error {
	if err := checkOverride(name); err != nil {
		return err
	}

	if strings.HasPrefix(name, targetPrefix) {
		return fmt.Errorf("%q is reserved for built-in variables; use --set to override %s*",
			name, targetPrefix)
	}

	return nil
}

// builtinVariables returns the built-ins for a host; targets default to it.
func builtinVariables(host platform) map[string]any {
	return map[string]any{
		varHostOS:    host.os,
		varHostCPU:   host.cpu,
		varTargetOS:  host.os,
		varTargetCPU: host.cpu,
	}
}

// parseOverride converts a --set value. "true" and "false" become booleans;
// anything else stays a string.
func parseOverride(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	default:
		return value
	}
}

// variables returns the built-in variables with saved --set overrides applied.
func variables(overrides map[string]string) (map[string]any, error) {
	vars := builtinVariables(hostPlatform())

	for name, value := range overrides {
		if err := checkOverride(name); err != nil {
			return nil, err
		}

		vars[name] = parseOverride(value)
	}

	return vars, nil
}
