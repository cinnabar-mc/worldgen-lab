package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSeedIdentity prevents mixing terrain from different seeds or incomplete metadata.
func TestSeedIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := checkSeed(dir, 42); err != nil {
		t.Fatal(err)
	}
	if err := checkSeed(dir, 42); err != nil {
		t.Fatal(err)
	}
	if err := checkSeed(dir, 43); err == nil {
		t.Error("different seed accepted")
	}
	for _, text := range []string{"{}", `{"seed":42}`, `{"generator":"steel-da3fb73f-geyser-81700ab5-stream-v1"}`} {
		if err := os.WriteFile(filepath.Join(dir, "generator.json"), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		if err := checkSeed(dir, 42); err == nil {
			t.Errorf("incomplete metadata accepted: %s", text)
		}
	}
	old := t.TempDir()
	if err := os.MkdirAll(filepath.Join(old, "world", "db"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := checkSeed(old, 42); err == nil {
		t.Error("world without metadata accepted")
	}
}

// TestSavedWorldValidation prevents a mistyped save path from becoming a new empty world.
func TestSavedWorldValidation(t *testing.T) {
	dir := t.TempDir()
	if err := checkSavedWorld(dir); err == nil {
		t.Fatal("accepted a directory without a world")
	}
	level := filepath.Join(dir, "level.dat")
	if err := os.WriteFile(level, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkSavedWorld(dir); err == nil {
		t.Fatal("accepted a world without its database")
	}
	db := filepath.Join(dir, "db")
	if err := os.WriteFile(db, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkSavedWorld(dir); err == nil {
		t.Fatal("accepted a database path that is a file")
	}
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(db, 0755); err != nil {
		t.Fatal(err)
	}
	if err := checkSavedWorld(dir); err != nil {
		t.Fatal(err)
	}
}
