package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/spf13/cobra"
)

func buildOpsCommands() (*cobra.Command, *cobra.Command) {
	serviceCmd := &cobra.Command{
		Use:   "service",
		Short: "Install and manage sablecourtd as a background service (macOS launchd or Ubuntu systemd)",
	}
	var svcManifests, svcDB, svcListen string
	serviceInstallCmd := &cobra.Command{
		Use:   "install",
		Short: "Install sablecourtd as a user background service on macOS (launchd) or Linux/Ubuntu (systemd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			binPath, err := exec.LookPath("sablecourtd")
			if err != nil {
				return fmt.Errorf("sablecourtd not found on PATH: %w", err)
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			dataDir := filepath.Join(home, ".local", "share", "sablecourt")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return err
			}
			if svcDB == "" {
				svcDB = filepath.Join(dataDir, "sablecourt.db")
			}
			if svcManifests == "" {
				svcManifests = filepath.Join(home, ".config", "sablecourt", "manifests")
			}

			switch goruntime.GOOS {
			case "darwin":
				plistDir := filepath.Join(home, "Library", "LaunchAgents")
				if err := os.MkdirAll(plistDir, 0o755); err != nil {
					return err
				}
				plistPath := filepath.Join(plistDir, "io.shadewright.sablecourtd.plist")
				plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.shadewright.sablecourtd</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>--mode=sanctum</string>
    <string>--listen=%s</string>
    <string>--db=%s</string>
    <string>--manifests=%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, binPath, svcListen, svcDB, svcManifests, filepath.Join(dataDir, "sablecourtd.out.log"), filepath.Join(dataDir, "sablecourtd.err.log"))
				if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
					return err
				}
				_ = exec.Command("launchctl", "unload", plistPath).Run()
				if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
					return fmt.Errorf("launchctl load failed: %s (%w)", strings.TrimSpace(string(out)), err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s installed macOS LaunchAgent %s\n", stJade.Render("✓"), plistPath)
				return nil

			case "linux":
				unitDir := filepath.Join(home, ".config", "systemd", "user")
				if err := os.MkdirAll(unitDir, 0o755); err != nil {
					return err
				}
				unitPath := filepath.Join(unitDir, "sablecourtd.service")
				unit := fmt.Sprintf(`[Unit]
Description=Sablecourt local control plane daemon
After=network-online.target

[Service]
Type=simple
ExecStart=%s --mode=sanctum --listen=%s --db=%s --manifests=%s
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`, binPath, svcListen, svcDB, svcManifests)
				if err := os.WriteFile(unitPath, []byte(unit), 0o600); err != nil {
					return err
				}
				_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
				if out, err := exec.Command("systemctl", "--user", "enable", "--now", "sablecourtd.service").CombinedOutput(); err != nil {
					return fmt.Errorf("systemctl enable failed: %s (%w)", strings.TrimSpace(string(out)), err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s installed Ubuntu/Linux systemd user service %s\n", stJade.Render("✓"), unitPath)
				return nil

			default:
				return fmt.Errorf("unsupported OS %q for service install", goruntime.GOOS)
			}
		},
	}
	serviceInstallCmd.Flags().StringVar(&svcListen, "listen", "127.0.0.1:7432", "Daemon listen address")
	serviceInstallCmd.Flags().StringVar(&svcDB, "db", "", "SQLite database path")
	serviceInstallCmd.Flags().StringVar(&svcManifests, "manifests", "", "Manifests directory or file")
	serviceCmd.AddCommand(serviceInstallCmd)

	tsCmd := &cobra.Command{
		Use:   "tailscale",
		Short: "Expose local sablecourtd privately to your Tailscale tailnet via tailscale serve",
	}
	var tsPort int
	tsExposeCmd := &cobra.Command{
		Use:   "expose",
		Short: "Expose local sablecourtd over HTTPS inside your Tailscale tailnet (no public port)",
		RunE: func(cmd *cobra.Command, args []string) error {
			tsBin, err := exec.LookPath("tailscale")
			if err != nil {
				return fmt.Errorf("tailscale CLI not found on PATH: %w", err)
			}
			target := fmt.Sprintf("http://127.0.0.1:%d", tsPort)
			if out, err := exec.Command(tsBin, "serve", "--bg", "--https=443", target).CombinedOutput(); err != nil {
				return fmt.Errorf("tailscale serve failed: %s (%w)", strings.TrimSpace(string(out)), err)
			}
			statusOut, _ := exec.Command(tsBin, "serve", "status").CombinedOutput()
			fmt.Fprintf(cmd.OutOrStdout(), "%s exposed %s on your Tailscale tailnet:\n%s\n", stJade.Render("✓"), target, strings.TrimSpace(string(statusOut)))
			return nil
		},
	}
	tsExposeCmd.Flags().IntVar(&tsPort, "port", 7432, "Local sablecourtd port")
	tsCmd.AddCommand(tsExposeCmd)
	tsCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show current Tailscale serve status",
		RunE: func(cmd *cobra.Command, args []string) error {
			tsBin, err := exec.LookPath("tailscale")
			if err != nil {
				return fmt.Errorf("tailscale CLI not found on PATH: %w", err)
			}
			out, err := exec.Command(tsBin, "serve", "status").CombinedOutput()
			if err != nil {
				return fmt.Errorf("tailscale serve status: %s (%w)", strings.TrimSpace(string(out)), err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(string(out)))
			return nil
		},
	})

	return serviceCmd, tsCmd
}
