package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const unitName = "pkgreg.service"

var hostnameListRE = regexp.MustCompile(`^[A-Za-z0-9.,:_-]*$`)

func runSystemd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, `usage: pkgreg systemd install [flags]

Copies this binary, installs a hardened systemd unit, initializes its state on
first start, and enables it. This is the clean-host one-command installation path.`)
		return nil
	}
	if args[0] != "install" {
		return fmt.Errorf("systemd: unknown action %q (want install)", args[0])
	}
	fs := flag.NewFlagSet("systemd install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	unitDir := fs.String("unit-dir", "/etc/systemd/system", "systemd unit directory")
	binDir := fs.String("bin-dir", "/usr/local/bin", "binary installation directory")
	dataDir := fs.String("data-dir", "/var/lib/pkgreg", "systemd StateDirectory path")
	hostnames := fs.String("hostnames", "", "extra certificate names/IPs (comma-separated)")
	start := fs.Bool("start", true, "daemon-reload and enable --now")
	force := fs.Bool("force", false, "replace a different installed binary or unit")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"unit-dir": *unitDir, "bin-dir": *binDir, "data-dir": *dataDir,
	} {
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("systemd: %s must be an absolute single-line path", name)
		}
	}
	if filepath.Dir(filepath.Clean(*dataDir)) != "/var/lib" ||
		!safeUnitName(filepath.Base(*dataDir)) {
		return errors.New("systemd: data-dir must be one safe directory directly under /var/lib")
	}
	if !hostnameListRE.MatchString(*hostnames) {
		return errors.New("systemd: hostnames contains unsupported characters")
	}
	current, err := os.Executable()
	if err != nil {
		return err
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return err
	}
	target := filepath.Join(*binDir, "pkgreg")
	if err := installExecutable(current, target, *force); err != nil {
		return err
	}
	unitPath := filepath.Join(*unitDir, unitName)
	unit := systemdUnit(target, *dataDir, *hostnames)
	if err := installText(unitPath, []byte(unit), 0o644, *force); err != nil {
		return err
	}
	fmt.Printf("installed binary  %s\ninstalled unit    %s\n", target, unitPath)
	if !*start {
		fmt.Println("not started (-start=false)")
		return nil
	}
	if err := runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl(ctx, "enable", "--now", unitName); err != nil {
		return err
	}
	fmt.Printf("enabled and started %s\nconsole: https://<host>:8443/\n", unitName)
	return nil
}

func systemdUnit(binary, dataDir, hostnames string) string {
	initArgs := binary + " init -data-dir " + dataDir
	if hostnames != "" {
		initArgs += " -hostnames " + hostnames
	}
	return `[Unit]
Description=pkgreg package registry and air-gap cache
Documentation=https://github.com/brightskies/package-registry
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
DynamicUser=yes
	StateDirectory=` + filepath.Base(dataDir) + `
UMask=0027
ExecStartPre=` + initArgs + `
ExecStart=` + binary + ` serve -config ` + filepath.Join(dataDir, "pkgreg.yaml") + `
Restart=on-failure
RestartSec=2s
TimeoutStopSec=2min
LimitNOFILE=1048576
NoNewPrivileges=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictRealtime=yes
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
`
}

func safeUnitName(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\`) &&
		strings.IndexFunc(value, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
				(r < '0' || r > '9') && r != '-' && r != '_'
		}) == -1
}

func installExecutable(source, target string, force bool) error {
	if same, err := sameContent(source, target); err == nil && same {
		return nil
	} else if err == nil && !force {
		return fmt.Errorf("systemd: %s already contains a different binary (pass -force)", target)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	temp, err := os.CreateTemp(filepath.Dir(target), ".pkgreg-install-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.Copy(temp, in); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return err
	}
	return os.Rename(name, target)
}

func installText(target string, body []byte, mode os.FileMode, force bool) error {
	existing, err := os.ReadFile(target)
	if err == nil {
		if bytes.Equal(existing, body) {
			return nil
		}
		if !force {
			return fmt.Errorf("systemd: %s already differs (pass -force)", target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".pkgreg-unit-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, target)
}

func sameContent(left, right string) (bool, error) {
	leftFile, err := os.Open(left)
	if err != nil {
		return false, err
	}
	defer func() { _ = leftFile.Close() }()
	rightFile, err := os.Open(right)
	if err != nil {
		return false, err
	}
	defer func() { _ = rightFile.Close() }()
	leftHash, rightHash := sha256.New(), sha256.New()
	if _, err := io.Copy(leftHash, leftFile); err != nil {
		return false, err
	}
	if _, err := io.Copy(rightHash, rightFile); err != nil {
		return false, err
	}
	return bytes.Equal(leftHash.Sum(nil), rightHash.Sum(nil)), nil
}

func runSystemctl(ctx context.Context, arguments ...string) error {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return errors.New("systemd: systemctl is unavailable; use -start=false and start the unit manually")
	}
	command := exec.CommandContext(ctx, path, arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %w: %s",
			strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
