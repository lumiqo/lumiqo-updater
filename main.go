//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	managerAPI       = "https://api.github.com/repos/lumiqo/lumiqo-manager"
	managerPublic    = "https://github.com/lumiqo/lumiqo-manager"
	binaryName       = "lumiqo-manager"
	maxBinary        = 128 << 20
	updateTimeout    = 3 * time.Minute
	reconcileTimeout = 20 * time.Minute
)

var stableTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type release struct {
	public     bool
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"assets"`
}

type releases struct {
	client *http.Client
	api    string
	token  string
	public string
}

func (r releases) get(ctx context.Context, base, path, accept, token string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		// URL errors can contain a signed asset URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return resp.Body, nil
}

func (r releases) publicLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.public+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := *r.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", fmt.Errorf("expected latest release redirect, got %s", resp.Status)
	}
	target, err := resp.Location()
	if err != nil {
		return "", err
	}
	prefix := strings.TrimSuffix(req.URL.Path, "/latest") + "/tag/"
	tag, found := strings.CutPrefix(target.Path, prefix)
	if target.Scheme != req.URL.Scheme || target.Host != req.URL.Host || target.User != nil ||
		!found || !stableTag.MatchString(tag) {
		return "", errors.New("unexpected latest release redirect")
	}
	return tag, nil
}

func (r releases) apiRelease(ctx context.Context, path string) (release, error) {
	body, err := r.get(ctx, r.api, path, "application/vnd.github+json", r.token)
	if err != nil {
		return release{}, err
	}
	defer body.Close()
	var rel release
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&rel); err != nil {
		return release{}, err
	}
	if rel.Draft || rel.Prerelease || !stableTag.MatchString(rel.Tag) {
		return release{}, fmt.Errorf("unexpected stable Manager release %q", rel.Tag)
	}
	return rel, nil
}

func (r releases) latest(ctx context.Context) (release, error) {
	if r.public != "" {
		tag, err := r.publicLatest(ctx)
		if err == nil {
			return release{Tag: tag, public: true}, nil
		}
		log.Printf("public Manager release lookup failed; trying GitHub API: %v", err)
	}
	return r.apiRelease(ctx, "/releases/latest")
}

func (r releases) asset(ctx context.Context, rel *release, name string) (io.ReadCloser, error) {
	if rel.public {
		body, err := r.get(ctx, r.public, "/releases/download/"+rel.Tag+"/"+name, "application/octet-stream", "")
		if err == nil {
			return body, nil
		}
		log.Printf("public Manager asset download failed; trying GitHub API: %v", err)
		apiRel, apiErr := r.apiRelease(ctx, "/releases/tags/"+rel.Tag)
		if apiErr != nil {
			return nil, apiErr
		}
		if apiRel.Tag != rel.Tag {
			return nil, errors.New("GitHub API returned a different Manager release tag")
		}
		*rel = apiRel
	}
	var id int64
	for _, asset := range rel.Assets {
		if asset.Name == name {
			if id != 0 || asset.ID <= 0 {
				return nil, fmt.Errorf("invalid asset metadata for %s", name)
			}
			id = asset.ID
		}
	}
	if id == 0 {
		return nil, fmt.Errorf("release %s is missing %s", rel.Tag, name)
	}
	return r.get(ctx, r.api, fmt.Sprintf("/releases/assets/%d", id), "application/octet-stream", r.token)
}

func newer(candidate, current string) bool {
	a := strings.Split(candidate[1:], ".")
	b := strings.Split(current[1:], ".")
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) > len(b[i])
		}
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func checksum(body io.Reader, name string) (string, error) {
	const limit = 64 << 10
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		return "", errors.New("SHA256SUMS exceeds size limit")
	}
	var digest string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size || digest != "" {
			return "", fmt.Errorf("invalid checksum for %s", name)
		}
		digest = strings.ToLower(fields[0])
	}
	if digest == "" {
		return "", fmt.Errorf("SHA256SUMS is missing %s", name)
	}
	return digest, nil
}

func verify(dst io.Writer, src io.Reader, expected string) error {
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(src, maxBinary+1))
	if err != nil {
		return err
	}
	if size == 0 || size > maxBinary || hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("Manager binary size or SHA-256 mismatch")
	}
	return nil
}

func activate(ctx context.Context, root, target, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, target, binaryName), "version")
	// The release binary only needs to report its version.
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("Manager version probe: %w", err)
	}
	var report struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(output, &report); err != nil || report.Version != tag {
		return fmt.Errorf("Manager did not report version %s", tag)
	}
	temp, err := os.MkdirTemp(root, ".activate-")
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	link := filepath.Join(temp, "current")
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	defer os.Remove(link)
	return os.Rename(link, filepath.Join(root, "current"))
}

func update(ctx context.Context, root, arch string, r releases, repairTimeout time.Duration) error {
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported architecture %s", arch)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".update.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			log.Print("another update is running")
			return nil
		}
		return err
	}
	updateCtx, cancel := context.WithTimeout(ctx, updateTimeout)
	err = updateManager(updateCtx, root, arch, r)
	cancel()
	if err != nil {
		return err
	}
	return reconcileManager(ctx, root, repairTimeout)
}

func updateManager(ctx context.Context, root, arch string, r releases) error {
	current, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		return fmt.Errorf("read Manager current link: %w", err)
	}
	currentTag := filepath.Base(current)
	if current != filepath.Join("versions", currentTag) || !stableTag.MatchString(currentTag) {
		return fmt.Errorf("unexpected Manager current target %q, expected versions/vMAJOR.MINOR.PATCH", current)
	}

	rel, err := r.latest(ctx)
	if err != nil {
		return err
	}
	if !newer(rel.Tag, currentTag) {
		log.Printf("Manager %s is current", currentTag)
		return nil
	}

	name := binaryName + "-linux-" + arch
	body, err := r.asset(ctx, &rel, "SHA256SUMS")
	if err != nil {
		return err
	}
	digest, err := checksum(body, name)
	body.Close()
	if err != nil {
		return err
	}
	target := filepath.Join("versions", rel.Tag)
	versionDir := filepath.Join(root, target)
	existing, err := os.Open(filepath.Join(versionDir, binaryName))
	switch {
	case err == nil:
		err = verify(io.Discard, existing, digest)
		existing.Close()
		if err != nil {
			return fmt.Errorf("existing Manager %s: %w", rel.Tag, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := stage(ctx, root, versionDir, name, digest, &rel, r); err != nil {
			return err
		}
	default:
		return err
	}
	if err := activate(ctx, root, target, rel.Tag); err != nil {
		return err
	}
	log.Printf("Manager updated from %s to %s", currentTag, rel.Tag)
	return nil
}

func reconcileManager(ctx context.Context, root string, timeout time.Duration) error {
	log.Print("applying Manager environment differences")
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, "current", binaryName), "reconcile", "--apply")
	cmd.Dir = "/"
	cmd.Env = []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"LANG=C.UTF-8",
		"DEBIAN_FRONTEND=noninteractive",
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Cancel the whole command group, including apt and its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("Manager reconcile --apply timed out after %s: %w", timeout, ctx.Err())
		}
		return fmt.Errorf("Manager reconcile --apply: %w", err)
	}
	return nil
}

func stage(ctx context.Context, root, versionDir, name, digest string, rel *release, r releases) error {
	dir, err := os.MkdirTemp(filepath.Join(root, "versions"), ".download-")
	if err != nil {
		return err
	}
	defer os.Remove(dir)
	path := filepath.Join(dir, binaryName)
	defer os.Remove(path)
	body, err := r.asset(ctx, rel, name)
	if err != nil {
		return err
	}
	defer body.Close()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	err = verify(file, body, digest)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}
	return os.Rename(dir, versionDir)
}

func main() {
	root := flag.String("manager-dir", "/opt/lumiqo/manager", "Manager installation directory")
	flag.Parse()
	if !filepath.IsAbs(*root) || flag.NArg() != 0 {
		log.Fatal("expected an absolute Manager installation directory and no positional arguments")
	}
	if os.Geteuid() != 0 {
		log.Fatal("Updater requires root")
	}
	client := &http.Client{
		Timeout: time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 5 {
				return errors.New("invalid release redirect")
			}
			req.Header.Del("Authorization")
			return nil
		},
	}
	source := releases{client: client, api: managerAPI, token: os.Getenv("GH_TOKEN"), public: managerPublic}
	if err := update(context.Background(), *root, runtime.GOARCH, source, reconcileTimeout); err != nil {
		log.Fatal(err)
	}
}
