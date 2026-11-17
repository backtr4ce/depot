package main

import "runtime"

// platform identifies an operating system and CPU using GN names, which are
// also the names gclient uses for host_os/host_cpu.
type platform struct {
	os  string
	cpu string
}

// gnOS maps every GOOS value to its GN target_os name.
var gnOS = map[string]string{
	"aix":       "aix",
	"android":   "android",
	"darwin":    "mac",
	"dragonfly": "dragonfly",
	"freebsd":   "freebsd",
	"illumos":   "illumos",
	"ios":       "ios",
	"js":        "js",
	"linux":     "linux",
	"netbsd":    "netbsd",
	"openbsd":   "openbsd",
	"plan9":     "plan9",
	"solaris":   "solaris",
	"wasip1":    "wasi",
	"windows":   "win",
}

// gnCPU maps every GOARCH value to its GN target_cpu name.
var gnCPU = map[string]string{
	"386":      "x86",
	"amd64":    "x64",
	"arm":      "arm",
	"arm64":    "arm64",
	"loong64":  "loong64",
	"mips":     "mips",
	"mipsle":   "mipsel",
	"mips64":   "mips64",
	"mips64le": "mips64el",
	"ppc64":    "ppc64",
	"ppc64le":  "ppc64",
	"riscv64":  "riscv64",
	"s390x":    "s390x",
	"wasm":     "wasm",
}

// newPlatform converts Go platform names to GN names. Values Go adds in
// the future are passed through unchanged rather than rejected.
func newPlatform(goos, goarch string) platform {
	p := platform{
		os:  goos,
		cpu: goarch,
	}

	if name, ok := gnOS[goos]; ok {
		p.os = name
	}

	if name, ok := gnCPU[goarch]; ok {
		p.cpu = name
	}

	return p
}

// hostPlatform reports the platform depot was built for. A 32-bit build on a
// 64-bit kernel reports the 32-bit CPU, matching gclient's userland check.
func hostPlatform() platform {
	return newPlatform(runtime.GOOS, runtime.GOARCH)
}
