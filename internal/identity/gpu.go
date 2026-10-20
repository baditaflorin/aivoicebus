package identity

import (
	"os"
	"runtime"
	"sync"
)

// gpuCaps is the capability words for this machine's GPU, added to the caps label: "gpu" and
// what runs on it ("gpu metal" on Apple silicon, "gpu cuda" with an NVIDIA driver loaded,
// "gpu rocm" with AMD's compute driver), or "" when none is seen. Only cheap checks (the OS and
// architecture, device files), read once per process. It is a variable for tests.
var gpuCaps = sync.OnceValue(func() string {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "gpu metal"
	case runtime.GOOS != "linux":
		return ""
	case exists("/dev/nvidiactl") || exists("/dev/nvidia0"):
		return "gpu cuda"
	case exists("/dev/kfd"):
		return "gpu rocm"
	}
	return ""
})

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
