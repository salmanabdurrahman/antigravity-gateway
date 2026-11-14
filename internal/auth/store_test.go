// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestStoreEmptyLoad verifies that Load on a non-existent accounts file returns an empty slice without error.
func TestStoreEmptyLoad(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := NewStore(storePath)

	accounts, err := store.Load()
	if err != nil {
		t.Fatalf("Load on nonexistent file failed: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("expected 0 accounts, got %d", len(accounts))
	}
}

// TestStoreUpsertAndLoad verifies upserting new and existing accounts with case-insensitive email matching.
func TestStoreUpsertAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := NewStore(storePath)

	acc1 := &Account{
		Email:        "user1@example.com",
		RefreshToken: "refresh-1",
		ProjectID:    "project-1",
		AddedAt:      "2026-01-01T00:00:00Z",
	}

	isNew, err := store.Upsert(acc1)
	if err != nil {
		t.Fatalf("Upsert acc1 failed: %v", err)
	}
	if !isNew {
		t.Errorf("expected isNew=true for first insert")
	}

	// Verify file permissions 0600
	fi, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	perm := fi.Mode().Perm()
	if perm != config.FilePerm {
		t.Errorf("expected permission 0600 (0%o), got 0%o", config.FilePerm, perm)
	}

	// Update existing account (case-insensitive email matching)
	acc1Update := &Account{
		Email:        "USER1@example.com",
		RefreshToken: "refresh-1-updated",
		ProjectID:    "project-1-updated",
	}
	isNewUpdate, err := store.Upsert(acc1Update)
	if err != nil {
		t.Fatalf("Upsert update failed: %v", err)
	}
	if isNewUpdate {
		t.Errorf("expected isNew=false for update")
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 account, got %d", len(loaded))
	}
	if loaded[0].RefreshToken != "refresh-1-updated" {
		t.Errorf("expected updated refresh token, got %s", loaded[0].RefreshToken)
	}
	if loaded[0].AddedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("expected original AddedAt preserved, got %s", loaded[0].AddedAt)
	}
}

// TestStoreRemove verifies removing accounts by email address from disk.
func TestStoreRemove(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := NewStore(storePath)

	_, _ = store.Upsert(&Account{
		Email:        "user1@example.com",
		RefreshToken: "ref-1",
		ProjectID:    "proj-1",
	})
	_, _ = store.Upsert(&Account{
		Email:        "user2@example.com",
		RefreshToken: "ref-2",
		ProjectID:    "proj-2",
	})

	// Remove user1 case-insensitively
	removed, err := store.Remove("USER1@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if !removed {
		t.Errorf("expected removed=true")
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 account left, got %d", len(loaded))
	}
	if loaded[0].Email != "user2@example.com" {
		t.Errorf("expected user2 to remain, got %s", loaded[0].Email)
	}

	// Try removing again
	removedAgain, err := store.Remove("user1@example.com")
	if err != nil {
		t.Fatalf("second Remove failed: %v", err)
	}
	if removedAgain {
		t.Errorf("expected removedAgain=false")
	}
}

// TestStoreSetDisabled verifies toggling account disabled state without modifying other credentials.
func TestStoreSetDisabled(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := NewStore(storePath)

	_, _ = store.Upsert(&Account{
		Email:        "user@example.com",
		RefreshToken: "ref",
		ProjectID:    "proj",
	})

	err := store.SetDisabled("user@example.com", true, "invalid_grant")
	if err != nil {
		t.Fatalf("SetDisabled failed: %v", err)
	}

	loaded, _ := store.Load()
	if len(loaded) != 1 || !loaded[0].Disabled || loaded[0].LastError != "invalid_grant" {
		t.Errorf("unexpected account state after SetDisabled: %+v", loaded[0])
	}
}

// TestStoreModTime verifies updating modification timestamps on disk to support daemon hot-reloading.
func TestStoreModTime(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := NewStore(storePath)

	modTime, err := store.ModTime()
	if err != nil {
		t.Fatalf("ModTime on non-existent file failed: %v", err)
	}
	if !modTime.IsZero() {
		t.Errorf("expected zero time for nonexistent file, got %v", modTime)
	}

	_, _ = store.Upsert(&Account{
		Email:        "user@example.com",
		RefreshToken: "ref",
		ProjectID:    "proj",
	})

	modTime2, err := store.ModTime()
	if err != nil {
		t.Fatalf("ModTime after write failed: %v", err)
	}
	if modTime2.IsZero() {
		t.Errorf("expected non-zero mod time")
	}
}

// TestStoreConcurrentAccess verifies thread-safe concurrent reads and writes across multiple goroutines.
func TestStoreConcurrentAccess(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := NewStore(storePath)

	var wg sync.WaitGroup
	workers := 10

	for i := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			email := filepath.Join("worker", time.Now().Format("150405.000000")) + "@example.com"
			_, _ = store.Upsert(&Account{
				Email:        email,
				RefreshToken: "ref",
				ProjectID:    "proj",
			})
			_, _ = store.Load()
		}(i)
	}

	wg.Wait()

	accounts, err := store.Load()
	if err != nil {
		t.Fatalf("Load after concurrent workers failed: %v", err)
	}
	if len(accounts) == 0 {
		t.Errorf("expected accounts to be stored")
	}
}
