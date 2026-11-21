//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdate(t *testing.T) {
	t.Setenv("GH_TOKEN", "updater-only")
	const current = "versions/v1.9.0"
	good := fakeManager("v1.10.0", "exit 0")
	disabled := fakeManager("v1.10.0", `test "$(cat "${dir%/*}/service-home/.local/state/lumiqot/services/worker")" = disabled`)
	oldBinary := fakeManager("v1.9.0", "exit 0")
	repairFailure := fakeManager("v1.10.0", "exit 42")
	repairTimeout := fakeManager("v1.10.0", "sleep 60 & wait")

	for _, tt := range []struct {
		name, tag, binary, checksumOf, want string
		wantRepair, wantError               string
	}{
		{"upgrade", "v1.10.0", good, good, "versions/v1.10.0", "v1.10.0", ""},
		{"upgrade with Worker disabled", "v1.10.0", disabled, disabled, "versions/v1.10.0", "v1.10.0", ""},
		{"hash mismatch", "v1.10.0", "corrupt", good, current, "", "SHA-256 mismatch"},
		{"version probe failed", "v1.10.0", "#!/bin/sh\nexit 1\n", "#!/bin/sh\nexit 1\n", current, "", "version probe"},
		{"version mismatch", "v2.0.0", good, good, current, "", "did not report version"},
		{"no downgrade", "v1.8.0", good, good, current, "v1.9.0", ""},
		{"no update", "v1.9.0", good, good, current, "v1.9.0", ""},
		{"repair failure", "v1.10.0", repairFailure, repairFailure, "versions/v1.10.0", "v1.10.0", "exit status 42"},
		{"repair timeout", "v1.10.0", repairTimeout, repairTimeout, "versions/v1.10.0", "v1.10.0", "timed out"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			preference := filepath.Join(root, "service-home/.local/state/lumiqot/services/worker")
			if tt.name == "upgrade with Worker disabled" {
				if err := os.MkdirAll(filepath.Dir(preference), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(preference, []byte("disabled\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			oldDir := filepath.Join(root, current)
			if err := os.MkdirAll(oldDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(oldDir, binaryName), []byte(oldBinary), 0755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "current")
			if err := os.Symlink(current, link); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases/latest":
					fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"id":1,"name":"SHA256SUMS"},{"id":2,"name":"lumiqo-manager-linux-amd64"}]}`, tt.tag)
				case "/releases/assets/1":
					fmt.Fprintf(w, "%x  lumiqo-manager-linux-amd64\n", sha256.Sum256([]byte(tt.checksumOf)))
				case "/releases/assets/2":
					fmt.Fprint(w, tt.binary)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			source := releases{client: server.Client(), api: server.URL}
			timeout := 5 * time.Second
			if tt.name == "repair timeout" {
				timeout = 300 * time.Millisecond
			}
			err := update(context.Background(), root, "amd64", source, timeout)
			if tt.wantError == "" && err != nil || tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Fatalf("update returned %v, expected error containing %q", err, tt.wantError)
			}
			if tt.name == "repair timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout should wrap context deadline, got %v", err)
			}
			if tt.wantRepair == "" {
				checkRepairLog(t, root)
			} else {
				checkRepairLog(t, root, tt.wantRepair)
			}
			if tt.name == "upgrade with Worker disabled" {
				if data, err := os.ReadFile(preference); err != nil || string(data) != "disabled\n" {
					t.Fatalf("service preference = %q, %v", data, err)
				}
			}
			got, err := os.Readlink(link)
			if err != nil || got != tt.want {
				t.Fatalf("current = %q, %v, want %q", got, err, tt.want)
			}
			old, err := os.ReadFile(filepath.Join(oldDir, binaryName))
			if err != nil || string(old) != oldBinary {
				t.Fatalf("previous Manager = %q, %v", old, err)
			}
			if tt.name == "upgrade" || tt.name == "repair failure" {
				err := update(context.Background(), root, "amd64", source, timeout)
				if tt.wantError == "" && err != nil || tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
					t.Fatalf("repeating update: %v", err)
				}
				checkRepairLog(t, root, "v1.10.0", "v1.10.0")
			}
		})
	}
}

func fakeManager(tag, repair string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$*" in
  version) printf '%%s\n' '{"version":"%s"}' ;;
  'reconcile --apply')
    test -z "${GH_TOKEN+x}" || exit 99
    dir=${0%%/*}
    printf '%%s\n' '%s' >> "${dir%%/*}/repairs" || exit 98
    %s
    ;;
  *) exit 97 ;;
esac
`, tag, tag, repair)
}

func checkRepairLog(t *testing.T, root string, tags ...string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "repairs"))
	if len(tags) == 0 {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected reconcile log: %q, %v", data, err)
		}
		return
	}
	want := strings.Join(tags, "\n") + "\n"
	if err != nil || string(data) != want {
		t.Fatalf("reconcile log = %q, %v, want %q", data, err, want)
	}
}
