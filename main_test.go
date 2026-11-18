//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdate(t *testing.T) {
	const current = "versions/v1.9.0"
	good := "#!/bin/sh\nprintf '%s\\n' '{\"version\":\"v1.10.0\"}'\n"

	for _, tt := range []struct {
		name, tag, binary, checksumOf, want string
		fail                                bool
	}{
		{"upgrade", "v1.10.0", good, good, "versions/v1.10.0", false},
		{"hash mismatch", "v1.10.0", "corrupt", good, current, true},
		{"version probe failed", "v1.10.0", "#!/bin/sh\nexit 1\n", "#!/bin/sh\nexit 1\n", current, true},
		{"version mismatch", "v2.0.0", good, good, current, true},
		{"no downgrade", "v1.8.0", good, good, current, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			oldDir := filepath.Join(root, current)
			if err := os.MkdirAll(oldDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(oldDir, binaryName), []byte("old"), 0755); err != nil {
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
			err := update(context.Background(), root, "amd64", source)
			if (err != nil) != tt.fail {
				t.Fatalf("update returned %v, expected failure: %v", err, tt.fail)
			}
			got, err := os.Readlink(link)
			if err != nil || got != tt.want {
				t.Fatalf("current = %q, %v, want %q", got, err, tt.want)
			}
			old, err := os.ReadFile(filepath.Join(oldDir, binaryName))
			if err != nil || string(old) != "old" {
				t.Fatalf("previous Manager = %q, %v", old, err)
			}
			if tt.name == "upgrade" {
				if err := update(context.Background(), root, "amd64", source); err != nil {
					t.Fatalf("repeating update: %v", err)
				}
			}
		})
	}
}
