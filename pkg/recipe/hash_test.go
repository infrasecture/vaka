package recipe

import (
	"testing"
)

func TestEntryState(t *testing.T) {
	root, err := OpenSafeRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := root.WriteFileSync("plain.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileSync("tool.sh", []byte("hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("plain.txt", "alias"); err != nil {
		t.Fatal(err)
	}

	// sha256("hello")
	const helloSum = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

	got, err := EntryState(root, "plain.txt")
	if err != nil || got != helloSum {
		t.Fatalf("plain state = %q, %v; want %q", got, err, helloSum)
	}
	got, err = EntryState(root, "tool.sh")
	if err != nil || got != helloSum+"+x" {
		t.Fatalf("exec state = %q, %v; want %q", got, err, helloSum+"+x")
	}
	got, err = EntryState(root, "alias")
	if err != nil || got != "link:plain.txt" {
		t.Fatalf("link state = %q, %v", got, err)
	}
	if !IsLinkState(got) || IsLinkState(helloSum) {
		t.Fatal("IsLinkState misclassifies")
	}

	// A chmod flips the state: mode changes count as modifications.
	if err := root.Chmod("plain.txt", 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = EntryState(root, "plain.txt")
	if err != nil || got != helloSum+"+x" {
		t.Fatalf("post-chmod state = %q, %v", got, err)
	}

	if _, err := EntryState(root, "missing"); err == nil {
		t.Fatal("EntryState of a missing path must error")
	}
}
