package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCCFingerprintFor_Stable(t *testing.T) {
	ResetFingerprintCache()

	fp1 := CCFingerprintFor("test-key-1")
	fp2 := CCFingerprintFor("test-key-1")
	if fp1.Thumbmark != fp2.Thumbmark {
		t.Fatalf("thumbmark not stable across calls: %q vs %q", fp1.Thumbmark, fp2.Thumbmark)
	}

	fp3 := CCFingerprintFor("test-key-2")
	if fp3.Thumbmark == fp1.Thumbmark {
		t.Fatalf("different api keys produced identical thumbmarks: %q", fp1.Thumbmark)
	}

	if len(fp1.Thumbmark) != 64 {
		t.Errorf("thumbmark length = %d, want 64 hex chars", len(fp1.Thumbmark))
	}
}

func TestCCFingerprintFor_ComponentsShape(t *testing.T) {
	ResetFingerprintCache()
	fp := CCFingerprintFor("shape-test")

	required := []string{
		"machineIdHash", "macHashes", "osUserHash", "hostnameHash",
		"gitEmailHash", "platform", "arch", "osRelease", "cpuModel",
		"cpuCount", "memGiB", "isContainer", "runtime", "collectorVersion",
	}
	for _, key := range required {
		if _, ok := fp.Components[key]; !ok {
			t.Errorf("components missing required key %q", key)
		}
	}
	if fp.Components["runtime"] != "cli" {
		t.Errorf("runtime = %v, want \"cli\"", fp.Components["runtime"])
	}
	if fp.Components["collectorVersion"] != 1 {
		t.Errorf("collectorVersion = %v, want 1", fp.Components["collectorVersion"])
	}
}

func TestHashSignal_SaltAndNullSeparator(t *testing.T) {
	// Reference hash: SHA-256("command-code:device-fingerprint:v1" || 0x00 || "test")
	expected := sha256.Sum256([]byte(ccFingerprintSalt + "\x00" + "test"))
	got := hashSignal("TEST")
	if got != hex.EncodeToString(expected[:]) {
		t.Errorf("hashSignal(\"TEST\") = %q, want %q", got, hex.EncodeToString(expected[:]))
	}
	if hashSignal("") != "" {
		t.Errorf("hashSignal(\"\") should return empty")
	}
	if hashSignal("   ") != "" {
		t.Errorf("hashSignal whitespace-only should return empty")
	}
}

func TestBuildThumbmark_KnownVector(t *testing.T) {
	// Known vector: machineId="abc", one mac="02:11:22:33:44:55"
	// joined = "abc|02:11:22:33:44:55"
	// expected = SHA-256(salt || "\0machine\0" || joined)
	joined := "abc|02:11:22:33:44:55"
	expected := sha256.Sum256([]byte(ccFingerprintSalt + "\x00machine\x00" + joined))
	got := buildThumbmark(ccSignals{
		MachineID:    "abc",
		MACAddresses: []string{"02:11:22:33:44:55"},
	})
	if got != hex.EncodeToString(expected[:]) {
		t.Errorf("buildThumbmark = %q, want %q", got, hex.EncodeToString(expected[:]))
	}
}

func TestBuildThumbmark_EmptyMachineIDUsesHostname(t *testing.T) {
	// Empty machineId → fallback to hostname + cpuModel.
	joined := "myhost|Intel CPU"
	expected := sha256.Sum256([]byte(ccFingerprintSalt + "\x00machine\x00" + joined))
	got := buildThumbmark(ccSignals{
		MachineID: "",
		Hostname:  "myhost",
		CPUModel:  "Intel CPU",
	})
	if got != hex.EncodeToString(expected[:]) {
		t.Errorf("buildThumbmark fallback = %q, want %q", got, hex.EncodeToString(expected[:]))
	}
}

func TestCCSessionContextFor_Stable(t *testing.T) {
	ResetFingerprintCache()
	sc1 := CCSessionContextFor("stable-key")
	sc2 := CCSessionContextFor("stable-key")
	if sc1.WorkingDir != sc2.WorkingDir {
		t.Errorf("workingDir not stable: %q vs %q", sc1.WorkingDir, sc2.WorkingDir)
	}
	if sc1.CurrentBranch != sc2.CurrentBranch {
		t.Errorf("currentBranch not stable: %q vs %q", sc1.CurrentBranch, sc2.CurrentBranch)
	}
	if len(sc1.Structure) == 0 {
		t.Errorf("structure should not be empty")
	}
	if len(sc1.RecentCommits) != 3 {
		t.Errorf("recentCommits length = %d, want 3", len(sc1.RecentCommits))
	}
	if !sc1.IsGitRepo {
		t.Errorf("isGitRepo = false, want true")
	}
}

func TestCCSessionContextFor_PerKey(t *testing.T) {
	ResetFingerprintCache()
	sc1 := CCSessionContextFor("alpha")
	sc2 := CCSessionContextFor("beta")
	if sc1.WorkingDir == sc2.WorkingDir {
		t.Errorf("workingDir should differ per apiKey, both = %q", sc1.WorkingDir)
	}
}

func TestRecordFingerprintIfNeeded_PostsOnce(t *testing.T) {
	ResetFingerprintCache()

	var hits int32
	gotThumb := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		if got := r.Header.Get("User-Agent"); got != "cli" {
			t.Errorf("User-Agent = %q, want cli", got)
		}
		if got := r.Header.Get("x-command-code-version"); got != "0.40.11" {
			t.Errorf("x-command-code-version = %q, want 0.40.11", got)
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		select {
		case gotThumb <- strings.TrimSpace(string(buf[:n])):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for i := 0; i < 3; i++ {
		RecordFingerprintIfNeeded(srv.URL, "test-key")
	}

	// Allow the background goroutine a moment to fire.
	<-gotThumb

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("expected 1 POST, got %d", got)
	}
}

func TestEnsureFingerprintRecorded_Sync(t *testing.T) {
	ResetFingerprintCache()

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := EnsureFingerprintRecorded(t.Context(), srv.URL, "sync-key"); err != nil {
		t.Fatalf("EnsureFingerprintRecorded: %v", err)
	}
	if !strings.Contains(gotBody, `"thumbmark"`) {
		t.Errorf("recorded body missing thumbmark: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"components"`) {
		t.Errorf("recorded body missing components: %s", gotBody)
	}
}
