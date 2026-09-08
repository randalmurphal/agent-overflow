package app

import (
	"os"
	"os/exec"
	"runtime"

	"agent-overflow/internal/platform"
)

// RemoteCommandEnvironment describes where a command executes, which can differ
// from the desktop's OS. Executables are PATH discoveries, not version checks or
// proof that a GPU/driver works; agents can request those probes explicitly.
type RemoteCommandEnvironment struct {
	ExecutionOS  string            `json:"executionOS"`
	Architecture string            `json:"architecture"`
	HostOS       string            `json:"hostOS"`
	Environment  string            `json:"environment"`
	Distribution string            `json:"distribution,omitempty"`
	Executables  map[string]string `json:"executables"`
}

// RemoteCommandEnvironment reports cheap local facts without launching tools,
// loading provider credentials, contacting networks, or initializing GPU drivers.
//
//ao:scope terminal:operate
//ao:route selected
func (a *App) RemoteCommandEnvironment() RemoteCommandEnvironment {
	return remoteCommandEnvironment(runtime.GOOS, runtime.GOARCH, platform.IsWSL(), os.Getenv("WSL_DISTRO_NAME"), exec.LookPath)
}

func remoteCommandEnvironment(goos, arch string, wsl bool, distribution string, lookPath func(string) (string, error)) RemoteCommandEnvironment {
	result := RemoteCommandEnvironment{
		ExecutionOS: goos, Architecture: arch, HostOS: goos, Environment: "native",
		Executables: map[string]string{},
	}
	if goos == "linux" && wsl {
		result.HostOS, result.Environment, result.Distribution = "windows", "wsl", distribution
	}
	for _, name := range []string{"git", "sh", "bash", "pwsh", "powershell.exe", "python3", "python", "node", "pnpm", "npm", "go", "cargo", "cmake", "make", "docker", "nvidia-smi", "rocm-smi", "nvcc"} {
		if path, err := lookPath(name); err == nil {
			result.Executables[name] = path
		}
	}
	return result
}
