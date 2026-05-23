package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type systemdUnitOptions struct {
	BinaryPath string
	ConfigPath string
	User       string
}

func runInstallService() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("install-service 仅支持 Linux systemd")
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	cfgPath := strings.TrimSpace(config)
	if cfgPath == "" {
		cfgPath = "/etc/qbit-upload.yaml"
	}
	cfgPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return fmt.Errorf("解析配置文件路径失败: %w", err)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("读取服务配置文件失败 %s: %w", cfgPath, err)
	}

	name := strings.TrimSpace(serviceName)
	if name == "" {
		name = "qbit-upload"
	}
	unitPath := filepath.Join("/etc/systemd/system", name+".service")
	unit := buildSystemdUnit(systemdUnitOptions{
		BinaryPath: binaryPath,
		ConfigPath: cfgPath,
		User:       serviceUser,
	})

	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("写入 systemd 服务失败 %s: %w", unitPath, err)
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "--now", name + ".service"},
	} {
		cmd := exec.Command("systemctl", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("执行 systemctl %s 失败: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}

	fmt.Printf("已安装并启动服务: %s\n", unitPath)
	return nil
}

func buildSystemdUnit(opts systemdUnitOptions) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=qbit-upload watcher\n")
	b.WriteString("After=network.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	if strings.TrimSpace(opts.User) != "" {
		b.WriteString("User=" + opts.User + "\n")
	}
	b.WriteString("ExecStart=" + systemdArg(opts.BinaryPath) + " watch --config " + systemdArg(opts.ConfigPath) + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=10s\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

func systemdArg(value string) string {
	if !strings.ContainsAny(value, " \t\"'") {
		return value
	}
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
