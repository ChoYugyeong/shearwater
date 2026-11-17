package main

import (
	"fmt"
	"os"
	"os/user"
	"strings"
)

// MCP의 command와 args는 셸 명령문이 아니다. 경로를 따옴표로 조합하거나
// 토큰을 환경 변수에 붙이지 않는다. Windows에서는 WSL 배포판과 일반
// 사용자를 명시해 기본 배포판/root로 잘못 연결되는 일을 막는다.
func workflowMCPConfig(file, platform string) (map[string]any, error) {
	if _, err := loadWorkflow(file); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"mcp", "--workflow=" + file, "--allow-write"}
	if platform == "windows" {
		distro := os.Getenv("WSL_DISTRO_NAME")
		u, e := user.Current()
		if e != nil {
			return nil, e
		}
		if distro == "" || strings.ContainsAny(distro, "\r\n\x00") || u.Uid == "0" {
			return nil, fmt.Errorf("Windows MCP export must run in the selected WSL as the normal user")
		}
		args = append([]string{"-d", distro, "-u", u.Username, "--exec", exe}, args...)
		exe = "wsl.exe"
	} else if platform != "linux" {
		return nil, fmt.Errorf("platform must be linux or windows")
	}
	return map[string]any{"mcpServers": map[string]any{"shearwater": map[string]any{"command": exe, "args": args}}}, nil
}
