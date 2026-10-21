package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadAccountAliases(t *testing.T) {
	aliases, err := LoadAccountAliases("")
	if err != nil || aliases != nil {
		t.Fatalf("disabled aliases: %v, %v", aliases, err)
	}
	for _, tc := range []struct {
		name string
		body string
		want map[string]string
	}{
		{"names", "aliases:\n  SESA836035_seemu: 张三\n  account-b: 'Alice & Bob'\n", map[string]string{"sesa836035_seemu": "张三", "account-b": "Alice & Bob"}},
		{"shared name", "aliases:\n  account-a: Alice\n  account-b: Alice\n", map[string]string{"account-a": "Alice", "account-b": "Alice"}},
		{"empty mapping", "aliases: {}\n", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "account-aliases.yaml")
			if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadAccountAliases(file)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("aliases: %v, %v", got, err)
			}
		})
	}
}

func TestLoadAccountAliasesRejectsInvalidFiles(t *testing.T) {
	for _, file := range []string{filepath.Join(t.TempDir(), "missing.yaml"), t.TempDir()} {
		if _, err := LoadAccountAliases(file); err == nil {
			t.Fatalf("expected read error for %s", file)
		}
	}
	for _, tc := range []struct{ name, body string }{
		{"empty document", ""},
		{"missing mapping", "{}"},
		{"unknown field", "aliases: {}\nother: value"},
		{"list", "aliases: []"},
		{"malformed", "aliases: ["},
		{"multiple documents", "aliases: {}\n---\naliases: {}"},
		{"duplicate login", "aliases:\n  account-a: Alice\n  account-a: Bob"},
		{"case-insensitive duplicate", "aliases:\n  Account-A: Alice\n  account-a: Bob"},
		{"empty login", "aliases:\n  '': Alice"},
		{"login whitespace", "aliases:\n  ' account-a ': Alice"},
		{"non-string login", "aliases:\n  123: Alice"},
		{"empty name", "aliases:\n  account-a: ''"},
		{"blank name", "aliases:\n  account-a: '  '"},
		{"null name", "aliases:\n  account-a: null"},
		{"non-string name", "aliases:\n  account-a: 123"},
		{"nested name", "aliases:\n  account-a: {name: Alice}"},
		{"oversized", "aliases: {}\n" + strings.Repeat("#", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "account-aliases.yaml")
			if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if aliases, err := LoadAccountAliases(file); err == nil || aliases != nil {
				t.Fatalf("accepted invalid aliases: %v, %v", aliases, err)
			}
		})
	}
}
