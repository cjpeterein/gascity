package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestSessionMutationLocksArePerSession(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})

	go func() {
		err := withSessionMutationLock("session-a", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		if err != nil {
			t.Errorf("lock session-a: %v", err)
		}
	}()

	select {
	case <-firstEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session-a lock was not acquired")
	}

	go func() {
		err := withSessionMutationLock("session-b", func() error {
			close(secondEntered)
			return nil
		})
		if err != nil {
			t.Errorf("lock session-b: %v", err)
		}
	}()

	select {
	case <-secondEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session-b was blocked by unrelated session lock")
	}

	close(releaseFirst)
}

func TestStripResumeFlag(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		resumeFlag string
		sessionKey string
		want       string
	}{
		{
			name:       "removes resume flag and key",
			cmd:        "claude --model claude-opus-4-7 --resume abc-123",
			resumeFlag: "--resume",
			sessionKey: "abc-123",
			want:       "claude --model claude-opus-4-7",
		},
		{
			name:       "resume flag at end",
			cmd:        "claude --resume abc-123",
			resumeFlag: "--resume",
			sessionKey: "abc-123",
			want:       "claude",
		},
		{
			name:       "no resume flag in command",
			cmd:        "claude --model sonnet",
			resumeFlag: "--resume",
			sessionKey: "abc-123",
			want:       "claude --model sonnet",
		},
		{
			name:       "empty resume flag",
			cmd:        "claude --resume abc-123",
			resumeFlag: "",
			sessionKey: "abc-123",
			want:       "claude --resume abc-123",
		},
		{
			name:       "empty session key",
			cmd:        "claude --resume abc-123",
			resumeFlag: "--resume",
			sessionKey: "",
			want:       "claude --resume abc-123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripResumeFlag(tt.cmd, tt.resumeFlag, tt.sessionKey)
			if got != tt.want {
				t.Errorf("stripResumeFlag(%q, %q, %q) = %q, want %q",
					tt.cmd, tt.resumeFlag, tt.sessionKey, got, tt.want)
			}
		})
	}
}

func TestPreemptiveStaleKeyCommand(t *testing.T) {
	tmp := t.TempDir()
	sessionKey := "abc-123"
	workDir := "/Users/alice/emerald/.gc/worktrees/agentdeck/polecats/gastown.furiosa"

	makeBead := func(keys map[string]string) beads.Bead {
		md := map[string]string{
			"session_key": sessionKey,
			"work_dir":    workDir,
			"provider":    "claude",
			"resume_flag": "--resume",
		}
		for k, v := range keys {
			md[k] = v
		}
		return beads.Bead{Metadata: md}
	}

	writeTranscript := func(t *testing.T) {
		t.Helper()
		slug := sessionlog.ProjectSlug(workDir)
		dir := filepath.Join(tmp, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, sessionKey+".jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}

	cmd := "claude --resume " + sessionKey

	t.Run("returns fresh command when transcript missing", func(t *testing.T) {
		got := preemptiveStaleKeyCommand(makeBead(nil), cmd, []string{tmp})
		if got != "claude" {
			t.Errorf("got %q, want %q", got, "claude")
		}
	})

	t.Run("returns empty when transcript present", func(t *testing.T) {
		writeTranscript(t)
		got := preemptiveStaleKeyCommand(makeBead(nil), cmd, []string{tmp})
		if got != "" {
			t.Errorf("got %q, want empty (transcript present)", got)
		}
	})

	t.Run("skips when session_key unset", func(t *testing.T) {
		got := preemptiveStaleKeyCommand(makeBead(map[string]string{"session_key": ""}), cmd, []string{tmp})
		if got != "" {
			t.Errorf("got %q, want empty (no key)", got)
		}
	})

	t.Run("skips when work_dir unset", func(t *testing.T) {
		got := preemptiveStaleKeyCommand(makeBead(map[string]string{"work_dir": ""}), cmd, []string{tmp})
		if got != "" {
			t.Errorf("got %q, want empty (no work_dir)", got)
		}
	})

	t.Run("skips for codex provider", func(t *testing.T) {
		got := preemptiveStaleKeyCommand(makeBead(map[string]string{"provider": "codex"}), cmd, []string{t.TempDir()})
		if got != "" {
			t.Errorf("got %q, want empty (codex unsupported for keyed lookup)", got)
		}
	})

	t.Run("skips when resume flag not strippable", func(t *testing.T) {
		got := preemptiveStaleKeyCommand(makeBead(nil), "claude --model sonnet", []string{t.TempDir()})
		if got != "" {
			t.Errorf("got %q, want empty (flag not present in command)", got)
		}
	})

	t.Run("provider_kind takes precedence over provider", func(t *testing.T) {
		got := preemptiveStaleKeyCommand(
			makeBead(map[string]string{"provider": "codex", "provider_kind": "claude"}),
			cmd,
			[]string{t.TempDir()},
		)
		if got != "claude" {
			t.Errorf("got %q, want %q (provider_kind=claude should win)", got, "claude")
		}
	})
}

func TestSessionMutationLocksSerializeSameSession(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})

	go func() {
		err := withSessionMutationLock("shared-session", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		if err != nil {
			t.Errorf("first lock: %v", err)
		}
	}()

	select {
	case <-firstEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first lock was not acquired")
	}

	go func() {
		err := withSessionMutationLock("shared-session", func() error {
			close(secondEntered)
			return nil
		})
		if err != nil {
			t.Errorf("second lock: %v", err)
		}
	}()

	select {
	case <-secondEntered:
		t.Fatal("same-session lock should block until the first holder releases")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)

	select {
	case <-secondEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("same-session lock did not unblock after release")
	}
}
