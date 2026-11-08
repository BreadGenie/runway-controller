// cache-node wraps only the runner's external Node executables.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const route = "http://127.0.0.1:18080/"

var bundles = loadAllowlist(allowlistData)

func candidate(script string) string {
	for _, name := range []string{"cache", "setup-node"} {
		if strings.Contains(script, "/_actions/actions/"+name+"/") {
			return name
		}
	}
	return ""
}

// supported returns the script's sha256 ("" when unreadable) and whether it is
// a pinned bundle.
func supported(script, name string) (string, bool) {
	f, err := os.OpenFile(script, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	h := sha256.New()
	// Pinned bundles are below 16 MiB. Bound reads of unexpected files.
	n, err := io.Copy(h, io.LimitReader(f, 16<<20+1))
	if err != nil || n > 16<<20 {
		return "", false
	}
	digest := hex.EncodeToString(h.Sum(nil))
	return digest, bundles[name][digest]
}

// unpinned says, in the step's own log, that a cache action fell back to
// GitHub's cache because its bundle is not allowlisted (e.g. a moved @v6 tag).
func unpinned(name, script, digest string) string {
	if digest == "" {
		digest = "unreadable"
	} else {
		digest = "sha256:" + digest[:12]
	}
	return fmt.Sprintf("runway-cache: actions/%s bundle %s (%s) is not on the regional-cache allowlist; this step uses GitHub's cache", name, digest, filepath.Base(script))
}

func routed(env []string) []string {
	result := make([]string, 0, len(env)+1)
	for _, value := range env {
		if !strings.HasPrefix(value, "ACTIONS_CACHE_URL=") && !strings.HasPrefix(value, "ACTIONS_CACHE_SERVICE_V2=") {
			result = append(result, value)
		}
	}
	return append(result, "ACTIONS_CACHE_URL="+route)
}

func environment(args, env []string, selectRoute func() bool, notify func(string)) []string {
	if len(args) < 2 {
		return env
	}
	name := candidate(args[1])
	if name == "" {
		return env
	}
	if digest, ok := supported(args[1], name); !ok {
		notify(unpinned(name, args[1], digest))
		return env
	}
	if selectRoute() {
		return routed(env)
	}
	return env
}

func selectRoute(directory string, getenv func(string) string, setup func(time.Time) bool) bool {
	// Job containers mount externals at /__e, without the guest route directory.
	if strings.HasPrefix(os.Args[0], "/__e/") {
		return false
	}
	key := ""
	for _, field := range []string{"GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_JOB"} {
		value := getenv(field)
		if value == "" {
			return false
		}
		key += value + "\x00"
	}
	digest := sha256.Sum256([]byte(key))
	path := filepath.Join(directory, hex.EncodeToString(digest[:]))
	deadline := time.Now().Add(4 * time.Second)
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK || time.Until(deadline) <= 0 {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if data, err := os.ReadFile(path); err == nil {
		return string(data) == "enabled\n"
	}
	enabled := setup(deadline)
	outcome := "disabled\n"
	if enabled {
		outcome = "enabled\n"
	}
	f, err := os.CreateTemp(directory, ".route-")
	if err != nil {
		return false
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(outcome)
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(f.Name(), path) != nil {
		return false
	}
	return enabled
}

type connection interface {
	io.ReadWriter
	SetDeadline(time.Time) error
}

func exchange(conn connection, deadline time.Time, getenv func(string) string) bool {
	if conn.SetDeadline(deadline) != nil {
		return false
	}
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return false
	}
	nonceBytes[6] = nonceBytes[6]&15 | 64
	nonceBytes[8] = nonceBytes[8]&63 | 128
	nonce := fmt.Sprintf("%x-%x-%x-%x-%x", nonceBytes[:4], nonceBytes[4:6], nonceBytes[6:8], nonceBytes[8:10], nonceBytes[10:])
	context := map[string]*string{}
	for _, field := range strings.Fields("repository repository_id run_id run_attempt ref ref_type sha event_name base_ref head_ref") {
		value := getenv("GITHUB_" + strings.ToUpper(field))
		if len(value) > 8192 || len([]rune(value)) > 2048 {
			return false
		}
		context[field] = &value
	}
	payload, err := json.Marshal(struct {
		Version int                `json:"version"`
		Nonce   string             `json:"nonce"`
		Context map[string]*string `json:"context"`
	}{2, nonce, context})
	if err != nil || len(payload)+1 > 32768 {
		return false
	}
	if _, err := io.Copy(conn, bytes.NewReader(append(payload, '\n'))); err != nil {
		return false
	}
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 4096 || reader.Buffered() != 0 {
		return false
	}
	// Compare a strict object so duplicates, extras and incorrect types fail closed.
	decoder := json.NewDecoder(bytes.NewReader(line))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	fields := map[string]any{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok {
			return false
		}
		if _, exists := fields[key]; exists {
			return false
		}
		var value any
		if decoder.Decode(&value) != nil {
			return false
		}
		fields[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return false
	}
	if _, err = decoder.Token(); err != io.EOF {
		return false
	}
	return len(fields) == 4 && fields["version"] == float64(2) && fields["nonce"] == nonce && fields["outcome"] == "enabled" && fields["route"] == route
}

func setup(deadline time.Time) bool {
	probe, err := net.DialTimeout("tcp", "127.0.0.1:18080", min(200*time.Millisecond, time.Until(deadline)))
	if err != nil {
		return false
	}
	probe.Close()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false
	}
	owned := true
	defer func() {
		if owned {
			unix.Close(fd)
		}
	}()
	err = unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: 1026})
	if err == unix.EINPROGRESS {
		timeout := min(time.Second, time.Until(deadline))
		if timeout <= 0 {
			return false
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, pollErr := unix.Poll(fds, int(timeout.Milliseconds()))
		if pollErr != nil || n != 1 {
			return false
		}
		code, sockErr := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if sockErr != nil || code != 0 {
			return false
		}
	} else if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), "vsock")
	owned = false
	defer file.Close()
	return exchange(file, minDeadline(deadline, time.Now().Add(3*time.Second)), os.Getenv)
}

func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func main() {
	env := environment(os.Args, os.Environ(), func() bool { return selectRoute("/run/runway", os.Getenv, setup) }, func(line string) { fmt.Fprintln(os.Stderr, line) })
	// The non-candidate path performs no filesystem reads before exec.
	real := filepath.Join(filepath.Dir(os.Args[0]), "node.real")
	if err := syscall.Exec(real, os.Args, env); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(127)
	}
}
