package main

// 처음 쓰는 사람에게 토큰을 읽어서 복사하고 chmod/chown을 하라고 요구하지
// 않는다. 관리자가 선택한 서버를 확인한 뒤 CA와 토큰만 일반 사용자에게
// 전달한다. 서버 개인 키/DB는 전달하지 않는다. 토큰은 여전히 관리자 권한이다.
// 앱 고정과 계획 승인은 실수 방지 장치이며 사용자별 권한 분리를 뜻하지 않는다.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type clientConnection struct {
	Format   string `json:"format"`
	Config   string `json:"config"`
	Listen   string `json:"listen"`
	Identity string `json:"identity"`
}

type clientTransfer struct {
	Connection clientConnection `json:"connection"`
	CA         []byte           `json:"ca"`
	Token      []byte           `json:"token"`
}

func userClientDir(home string) string {
	return filepath.Join(home, ".local", "state", "shearwater", "client")
}

func clientDefaults() (clientConnection, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return clientConnection{}, err
	}
	var saved clientConnection
	err = readPrivateJSON(filepath.Join(userClientDir(home), "connection.json"), &saved)
	if errors.Is(err, os.ErrNotExist) {
		return clientConnection{Config: defaultConfigDir, Listen: defaultListen}, nil
	}
	if err != nil {
		return saved, err
	}
	if saved.Format != "shearwater.client/v1" || saved.Config != userClientDir(home) || saved.Identity == "" {
		return saved, fmt.Errorf("invalid saved client connection")
	}
	if err = localListen(saved.Listen); err != nil {
		return saved, err
	}
	return saved, nil
}

func runSetup(args []string, out io.Writer) error {
	f := flag.NewFlagSet("setup", flag.ContinueOnError)
	var o opsOptions
	o.flags(f)
	apply := f.Bool("apply", false, "explicitly start if needed and delegate credentials (default: check only)")
	username := f.String("user", "", "normal Linux user who will run the AI client")
	destination := f.String("client-dir", "", "new user-owned client directory (default: user's ~/.local/state/shearwater/client)")
	if err := parseOps(f, args, &o); err != nil {
		return err
	}
	if *username == "" {
		return fmt.Errorf("setup requires --user with the normal Linux user; check with id -un before sudo")
	}
	u, err := user.Lookup(*username)
	if err != nil {
		return err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil || uid == 0 {
		return fmt.Errorf("AI credentials must belong to a normal user, not root")
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return err
	}
	if *destination == "" {
		*destination = userClientDir(u.HomeDir)
	}
	if !filepath.IsAbs(*destination) || filepath.Clean(*destination) == "/" {
		return fmt.Errorf("client-dir must be an absolute private directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c, connectionErr := localAPI(o.ConfigDir, o.Listen)
	if connectionErr == nil {
		connectionErr = checkSetupAPI(ctx, c)
	}
	if !*apply {
		// 조회만 한다. 이미 실행 중이면 준비물을 재설치하지 않는다.
		checks := []map[string]any{}
		if connectionErr != nil {
			for _, check := range hostChecks(ctx, o) {
				row := map[string]any{"check": check.name, "passed": check.err == nil}
				if check.err != nil {
					row["reason"] = check.err.Error()
					row["next"] = check.hint
				}
				checks = append(checks, row)
			}
		}
		return json.NewEncoder(out).Encode(map[string]any{"applied": false, "serverReady": connectionErr == nil, "user": u.Username, "clientDir": *destination, "checks": checks, "next": "review these paths, then run the same setup command with sudo and --apply; existing credentials and apps are never overwritten"})
	}
	if os.Geteuid() != 0 {
		return needRoot("start the node and delegate its credentials; run AI commands afterward as the selected normal user")
	}
	var denied *operationError
	if errors.As(connectionErr, &denied) && denied.code == "UNAUTHORIZED" {
		return connectionErr // 인증 실패를 새 서버 시작이나 토큰 교체로 해결하지 않는다.
	}
	if err = checkPrivateDir(o.ConfigDir, true); err != nil {
		return err
	}
	started := false
	if connectionErr != nil {
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		startArgs := []string{"up", "--listen=" + o.Listen, "--data-dir=" + o.DataDir, "--config-dir=" + o.ConfigDir, "--run-dir=" + o.RunDir, "--log-dir=" + o.LogDir, "--containerd-socket=" + o.Socket, "--node-name=" + o.NodeName, "--runtime=" + o.Runtime}
		// up에는 기존 인스턴스 소유권·포트·경로 검사와 시작 제한 시간이 있다.
		// 인증 오류를 기존 서버 삭제나 토큰 회전으로 '고치지' 않는다.
		p := exec.Command(exe, startArgs...)
		b, e := p.CombinedOutput()
		if e != nil {
			return fmt.Errorf("setup could not start server: %s", strings.TrimSpace(string(b)))
		}
		started = true
		c, err = localAPI(o.ConfigDir, o.Listen)
		if err != nil {
			return err
		}
	}
	identity, err := connectionIdentity(c, o.ConfigDir)
	if err != nil {
		return err
	}
	ca, err := readSetupFile(filepath.Join(o.ConfigDir, "ca.crt"))
	if err != nil {
		return err
	}
	token, err := readSetupFile(filepath.Join(o.ConfigDir, "admin.token"))
	if err != nil {
		return err
	}
	transfer := clientTransfer{Connection: clientConnection{Format: "shearwater.client/v1", Config: *destination, Listen: o.Listen, Identity: identity}, CA: ca, Token: token}
	b, err := json.Marshal(transfer)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "receive-client", "--client-dir="+*destination)
	// 자식이 실제 일반 사용자 권한으로 폴더를 만든다. root가 사용자 홈을
	// 따라가며 chown하는 방식과 달리 사용자 경로 교체로 root 쓰기를 유도할 수 없다.
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	cmd.Env = append(os.Environ(), "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
	cmd.Stdin = bytes.NewReader(b)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("server is available, but client setup failed (no rollback or app deletion): %s", strings.TrimSpace(string(output)))
	}
	return json.NewEncoder(out).Encode(map[string]any{"applied": true, "serverStarted": started, "serverReady": true, "user": u.Username, "clientDir": *destination, "authority": "local administrator token; no RBAC", "next": "as the normal user: sw connect --name YOUR_APP (existing), or add --image IMAGE (new); sw setup does not install automatic boot services"})
}

func readSetupFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Mode&0o022 != 0 {
		return nil, fmt.Errorf("unsafe credential source: %s", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	if len(b) > 65536 {
		return nil, fmt.Errorf("credential file too large")
	}
	return b, nil
}

// 이 내부 명령은 비밀을 명령줄/환경/로그로 받지 않는다. root가 만든 stdin
// 파이프만 사용한다. 직접 실행하더라도 이미 가진 토큰 이상의 권한은 얻지 않는다.
func receiveClient(args []string, in io.Reader, out io.Writer) error {
	f := flag.NewFlagSet("receive-client", flag.ContinueOnError)
	dir := f.String("client-dir", "", "private credentials destination")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || !filepath.IsAbs(*dir) || filepath.Clean(*dir) == "/" || os.Geteuid() == 0 {
		return fmt.Errorf("client import requires a normal user and an absolute directory")
	}
	b, err := io.ReadAll(io.LimitReader(in, 131073))
	if err != nil || len(b) > 131072 {
		return fmt.Errorf("invalid client transfer size")
	}
	var transfer clientTransfer
	if err = decodeStrict(b, &transfer); err != nil {
		return err
	}
	con := transfer.Connection
	if con.Format != "shearwater.client/v1" || con.Config != *dir || con.Identity == "" {
		return fmt.Errorf("client transfer identity mismatch")
	}
	if err = localListen(con.Listen); err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(transfer.CA) || len(bytes.TrimSpace(transfer.Token)) == 0 {
		return fmt.Errorf("invalid client credentials")
	}
	hash := sha256.New()
	hash.Write(transfer.CA)
	hash.Write(bytes.TrimSpace(transfer.Token))
	if fmt.Sprintf("%x", hash.Sum(nil)) != con.Identity {
		return fmt.Errorf("client transfer fingerprint mismatch")
	}
	if err = privateDir(*dir, true); err != nil {
		return err
	}
	info, err := os.Stat(*dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("client directory must be private (0700); choose a new --client-dir")
	}
	lock, err := fileLock(*dir, "setup.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	// 중단 뒤 다시 실행하면 동일한 내용만 보완한다. 다른 서버의 인증을
	// 덮어쓰지 않으며, 비교가 전부 끝나기 전에는 새 파일도 쓰지 않는다.
	files := []struct {
		name string
		data []byte
	}{{"ca.crt", transfer.CA}, {"admin.token", transfer.Token}}
	meta, _ := json.MarshalIndent(con, "", "  ")
	files = append(files, struct {
		name string
		data []byte
	}{"connection.json", append(meta, '\n')})
	for _, file := range files {
		path := filepath.Join(*dir, file.name)
		previous, e := openPrivate(path, unix.O_RDONLY)
		if e == nil {
			old, readErr := io.ReadAll(io.LimitReader(previous, 65537))
			previous.Close()
			if readErr != nil || !bytes.Equal(old, file.data) {
				return fmt.Errorf("existing client differs; use a new --client-dir and reconnect, never overwrite credentials")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	for _, file := range files {
		path := filepath.Join(*dir, file.name)
		dst, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(e, os.ErrExist) {
			continue
		}
		if e != nil {
			return e
		}
		_, e = dst.Write(file.data)
		if e == nil {
			e = dst.Sync()
		}
		dst.Close()
		if e != nil {
			return e
		}
	}
	c, err := localAPI(*dir, con.Listen)
	if err != nil {
		return err
	}
	identity, err := connectionIdentity(c, *dir)
	if err != nil || identity != con.Identity {
		return fmt.Errorf("delegated credentials identity mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = checkSetupAPI(ctx, c); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]bool{"clientReady": true})
}

// readyz는 설치 도구가 서버 시작을 기다릴 수 있도록 인증 없이도 열려 있다.
// 따라서 200 하나만 보고 '인증 완료'라고 하면 틀린 토큰도 통과하게 된다.
// 실제 인증이 필요한 목록 조회까지 성공해야 사용 가능한 연결로 보고한다.
func checkSetupAPI(ctx context.Context, c *agentClient) error {
	if err := c.get(ctx, "/readyz", nil); err != nil {
		return err
	}
	return c.get(ctx, "/api/v1/namespaces", nil)
}
