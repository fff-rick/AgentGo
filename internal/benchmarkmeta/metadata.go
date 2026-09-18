package benchmarkmeta

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type Metadata struct {
	Commit        string `json:"commit"`
	Dirty         bool   `json:"dirty"`
	DatasetSHA256 string `json:"dataset_sha256,omitempty"`
	CorpusSHA256  string `json:"corpus_sha256,omitempty"`
	Backend       string `json:"backend"`
	Scenario      string `json:"scenario"`
	Model         string `json:"model,omitempty"`
	ModelParams   string `json:"model_params,omitempty"`
	ConfigVersion string `json:"config_version,omitempty"`
	Machine       string `json:"machine"`
	GoVersion     string `json:"go_version"`
	MemoryBytes   uint64 `json:"memory_bytes,omitempty"`
}

func Collect(dataset, backend, scenario, model, params, config string) Metadata {
	var datasetHash string
	if dataset != "" {
		datasetHash = HashFile(dataset)
	}
	commit, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	dirty, _ := exec.Command("git", "status", "--porcelain").Output()
	host, _ := os.Hostname()
	return Metadata{Commit: strings.TrimSpace(string(commit)), Dirty: len(dirty) > 0, DatasetSHA256: datasetHash, Backend: backend, Scenario: scenario, Model: model, ModelParams: params, ConfigVersion: config, Machine: fmt.Sprintf("%s/%s %s (%d CPUs)", runtime.GOOS, runtime.GOARCH, host, runtime.NumCPU()), GoVersion: runtime.Version(), MemoryBytes: systemMemory()}
}
func HashFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum)
}
func systemMemory() uint64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var kib uint64
	if _, err := fmt.Sscanf(string(raw), "MemTotal: %d kB", &kib); err != nil {
		return 0
	}
	return kib * 1024
}
