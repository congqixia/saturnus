package review

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

type PRInfo struct {
	Title      string `json:"title"`
	BaseBranch string `json:"baseRefName"`
}

func ResolvePRInfo(ctx context.Context, ghCLI, repo string, num int) PRInfo {
	if ghCLI == "" {
		return PRInfo{}
	}
	cmd := exec.CommandContext(ctx, ghCLI, "pr", "view", strconv.Itoa(num), "--repo", repo, "--json", "title,baseRefName")
	out, err := cmd.Output()
	if err != nil {
		return PRInfo{}
	}
	var info PRInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return PRInfo{}
	}
	return info
}

func IsGitDir(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}
