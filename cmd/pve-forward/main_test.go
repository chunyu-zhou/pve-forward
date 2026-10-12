package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pveforward/internal/store"
)

func TestRuntimeEnvAndExplicitFlagPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                string
		args                []string
		envListen, envTLS   string
		wantListen, wantTLS string
		wantError           bool
	}{
		{name: "empty leaves DB settings"},
		{name: "env runtime overrides", envListen: ":5000", envTLS: "proxy", wantListen: ":5000", wantTLS: "proxy"},
		{name: "flags override env", args: []string{"-listen=:6000", "-tls=self"}, envListen: ":5000", envTLS: "proxy", wantListen: ":6000", wantTLS: "self"},
		{name: "empty flags restore DB settings", args: []string{"-listen=", "-tls="}, envListen: ":5000", envTLS: "invalid"},
		{name: "invalid env TLS", envTLS: "invalid", wantError: true},
		{name: "valid CLI masks invalid env TLS", args: []string{"-tls=proxy"}, envTLS: "invalid", wantTLS: "proxy"},
		{name: "invalid CLI TLS", args: []string{"-tls=invalid"}, envTLS: "proxy", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			listen := fs.String("listen", "", "")
			tlsMode := fs.String("tls", "", "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := applyRuntimeEnv(fs, func(key string) string {
				switch key {
				case "PVE_FORWARD_LISTEN":
					return tc.envListen
				case "PVE_FORWARD_TLS":
					return tc.envTLS
				default:
					t.Fatalf("unexpected env read: %s", key)
					return ""
				}
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v, wantError=%v", err, tc.wantError)
			}
			if err == nil && (*listen != tc.wantListen || *tlsMode != tc.wantTLS) {
				t.Fatalf("listen=%q TLS=%q, want %q/%q", *listen, *tlsMode, tc.wantListen, tc.wantTLS)
			}
		})
	}
}

func TestSetupTokenCommandIgnoresBootstrapEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PVE_FORWARD_ADMIN_USER", "invalid user")
	t.Setenv("PVE_FORWARD_ADMIN_PASSWORD", "short")
	t.Setenv("PVE_FORWARD_ADMIN_PASSWORD_FILE", filepath.Join(dir, "missing-secret"))
	t.Setenv("PVE_FORWARD_TLS", "invalid")
	t.Setenv("PVE_FORWARD_LISTEN", "invalid")
	oldArgs := os.Args
	os.Args = []string{"pve-forward", "setup-token", "-data", dir}
	t.Cleanup(func() { os.Args = oldArgs })
	main()
	if _, err := os.Stat(filepath.Join(dir, "setup-token")); err != nil {
		t.Fatalf("maintenance command did not create setup token: %v", err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Setting("setup_done", "") != "" {
		t.Fatal("maintenance command initialized admin")
	}
}

func TestOpenMasterKeyRefusesExistingDatabaseWithoutKey(t *testing.T) {
	for _, name := range []string{"data.db", "data.db-wal", "data.db-shm"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("existing database"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openMasterKey(dir); err == nil || !strings.Contains(err.Error(), "master.key") {
				t.Fatalf("expected missing-key restore error, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "master.key")); !os.IsNotExist(err) {
				t.Fatalf("master.key was created: %v", err)
			}
		})
	}
}

func TestOpenMasterKeyCreatesOnlyForFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	box, err := openMasterKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := box.Seal("preserve-encrypted-credentials")
	keyPath := filepath.Join(dir, "master.key")
	original, err := os.ReadFile(keyPath)
	if err != nil || len(original) != 32 {
		t.Fatalf("invalid new key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.db"), []byte("existing database"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := openMasterKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := reopened.Open(ciphertext)
	if err != nil || plain != "preserve-encrypted-credentials" {
		t.Fatal("key changed on restart")
	}
	current, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("master.key file changed")
	}
}

func TestOpenMasterKeyDoesNotReplaceMalformedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openMasterKey(dir); err == nil {
		t.Fatal("expected malformed-key error")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "invalid" {
		t.Fatal("malformed key was overwritten")
	}
}
