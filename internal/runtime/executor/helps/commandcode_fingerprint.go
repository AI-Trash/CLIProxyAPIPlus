package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CCCLIVersion is the official command-code CLI version we fingerprint as.
// Keep this in lockstep with https://www.npmjs.com/package/command-code (latest).
// Used for x-command-code-version on generate, fingerprint/record, and login.
// Synced from command-code@1.44.0 (package.json / dist/cli.mjs package version).
const CCCLIVersion = "1.44.0"

// ccFingerprintSalt is the device-fingerprint salt embedded in the official
// command-code CLI (>=0.40.x). It is publicly visible in the npm bundle and
// is required to compute a thumbmark that the server can verify against
// incoming /alpha/fingerprint/record submissions.
// Verified against command-code@1.44.0 dist/cli.mjs (MC="command-code:device-fingerprint:v1").
const ccFingerprintSalt = "command-code:device-fingerprint:v1"

// ccFingerprint is the payload posted to /alpha/fingerprint/record. The
// structure mirrors buildMachineFingerprint() in the official CLI.
type ccFingerprint struct {
	Thumbmark  string                 `json:"thumbmark"`
	Components map[string]interface{} `json:"components"`
}

// ccSignals holds the raw per-machine signals used to build a fingerprint.
// Empty fields are skipped during hashing (matching the official CLI which
// uses .filter(Boolean) on the joined array).
type ccSignals struct {
	MachineID    string
	MACAddresses []string
	OSUser       string
	Hostname     string
	GitEmail     string
	Platform     string
	Arch         string
	OSRelease    string
	CPUModel     string
	CPUCount     int
	TotalMemGiB  int
	IsContainer  bool
	Timezone     string
}

// hashSignal reproduces hashSignal() from the official CLI:
//
//	SHA-256(salt || "\0" || lowercased(trimmed(value)))
func hashSignal(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(ccFingerprintSalt))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(trimmed)))
	return hex.EncodeToString(h.Sum(nil))
}

// buildThumbmark reproduces buildMachineFingerprint().thumbmark from the
// official CLI:
//
//	SHA-256(salt || "\0machine\0" || joinedSignals)
//
// joinedSignals is built from the deduped, sorted, lowercased MAC list plus
// machineId and (when machineId is empty) hostname + cpuModel as fallbacks.
func buildThumbmark(signals ccSignals) string {
	macs := dedupSortedMACs(signals.MACAddresses)
	parts := []string{strings.TrimSpace(signals.MachineID), strings.Join(macs, ",")}
	if strings.TrimSpace(signals.MachineID) == "" {
		parts = append(parts, strings.TrimSpace(signals.Hostname))
		parts = append(parts, strings.TrimSpace(signals.CPUModel))
	}
	filtered := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	joined := strings.Join(filtered, "|")
	if joined == "" {
		joined = "unknown"
	}

	h := sha256.New()
	h.Write([]byte(ccFingerprintSalt))
	h.Write([]byte("\x00machine\x00"))
	h.Write([]byte(joined))
	return hex.EncodeToString(h.Sum(nil))
}

// dedupSortedMACs lowercases and de-duplicates MAC addresses, then sorts them
// alphabetically. Empty and zero-MAC values are dropped (matching the
// official CLI's filter).
func dedupSortedMACs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, m := range in {
		low := strings.ToLower(strings.TrimSpace(m))
		if low == "" || low == "00:00:00:00:00:00" {
			continue
		}
		if _, dup := seen[low]; dup {
			continue
		}
		seen[low] = struct{}{}
		out = append(out, low)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// buildFingerprint assembles the thumbmark + components object matching the
// shape of buildMachineFingerprint() in the official CLI.
func buildFingerprint(signals ccSignals) ccFingerprint {
	macs := dedupSortedMACs(signals.MACAddresses)
	macHashes := make([]string, 0, len(macs))
	for _, m := range macs {
		if h := hashSignal(m); h != "" {
			macHashes = append(macHashes, h)
		}
	}

	components := map[string]interface{}{
		"machineIdHash":    hashSignal(signals.MachineID),
		"macHashes":        macHashes,
		"osUserHash":       hashSignal(signals.OSUser),
		"hostnameHash":     hashSignal(signals.Hostname),
		"gitEmailHash":     hashSignal(signals.GitEmail),
		"platform":         signals.Platform,
		"arch":             signals.Arch,
		"osRelease":        signals.OSRelease,
		"cpuModel":         signals.CPUModel,
		"cpuCount":         signals.CPUCount,
		"memGiB":           signals.TotalMemGiB,
		"isContainer":      signals.IsContainer,
		"runtime":          "cli",
		"collectorVersion": 1,
	}
	if signals.Timezone != "" {
		components["timezone"] = signals.Timezone
	}

	return ccFingerprint{
		Thumbmark:  buildThumbmark(signals),
		Components: components,
	}
}

// ---------------------------------------------------------------------------
// Fake but plausible per-process signals. These are deterministically derived
// from the API key so they stay stable for the lifetime of the process but
// differ across accounts. Real values are not used because the proxy runs on
// the server host, not the user's workstation, so any "real" machine signals
// would be wrong and would change every restart.
// ---------------------------------------------------------------------------

var (
	ccRawSignalPool sync.Map // scope -> ccSignals  (shared by fingerprint + session)
	ccSignalPool    sync.Map // scope -> ccFingerprint
	ccSessionPool   sync.Map // scope -> *ccSessionContext
	ccSessionIDPool sync.Map // scope -> string (stable x-session-id per apiKey)
	ccRecordedKeys  sync.Map // scope -> struct{}
)

func ccKeyScope(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// randomHexToken returns n hex characters from the seeded RNG (stable).
func randomHexToken(rng *mrand.Rand, n int) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = hexdigits[rng.Intn(len(hexdigits))]
	}
	return string(out)
}

// randomMAC builds a locally-administered unicast MAC (02:xx:xx:xx:xx:xx).
// Matches the shape produced by Node's os.networkInterfaces().
func randomMAC(rng *mrand.Rand) string {
	return fmt.Sprintf("02:%s:%s:%s:%s:%s",
		randomHexToken(rng, 2), randomHexToken(rng, 2),
		randomHexToken(rng, 2), randomHexToken(rng, 2),
		randomHexToken(rng, 2))
}

// seededRand builds a math/rand RNG seeded from the given strings so that the
// same inputs produce the same sequence across calls within the process.
func seededRand(parts ...string) *mrand.Rand {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return mrand.New(mrand.NewSource(int64(sum[0])<<56 | int64(sum[1])<<48 | int64(sum[2])<<40 | int64(sum[3])<<32 |
		int64(sum[4])<<24 | int64(sum[5])<<16 | int64(sum[6])<<8 | int64(sum[7])))
}

// generateSignals builds a deterministic set of plausible CLI-side signals for
// the given apiKey. Values are intentionally fake so they don't leak the host
// running the proxy, but they are stable across requests so the upstream sees
// a coherent "machine fingerprint" for the session.
//
// All fields (including machineId and MACs) are derived from the seeded RNG so
// fingerprint and session-context builders share identical signals.
func generateSignals(apiKey string) ccSignals {
	rng := seededRand("cc-signals", apiKey)

	platform := "linux"
	arch := "x64"
	switch runtime.GOOS {
	case "darwin":
		platform = "darwin"
	case "windows":
		platform = "win32"
	}
	switch runtime.GOARCH {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	}

	cpuModels := []string{
		"Apple M2 Pro",
		"Apple M3 Max",
		"Intel(R) Core(TM) i7-13700K",
		"Intel(R) Core(TM) i9-14900K",
		"AMD Ryzen 9 7950X",
		"AMD Ryzen 7 7800X3D",
	}
	cpuModel := cpuModels[rng.Intn(len(cpuModels))]
	cpuCount := []int{8, 10, 12, 16, 20, 24, 32}[rng.Intn(7)]
	memGiB := []int{16, 32, 64, 96, 128}[rng.Intn(5)]

	osUsers := []string{"dev", "developer", "user", "engineer", "coder", "alex", "sam", "jess"}
	hostnames := []string{
		"macbook-pro-01", "workstation-01", "dev-laptop", "studio", "main-pc",
		"mbp-2023", "thinkpad-x1", "desktop-primary", "linux-build-01",
	}
	emails := []string{
		"dev@example.com", "user@example.org", "developer@users.noreply.github.com",
		"engineer@private.example", "coder@users.noreply.github.com",
	}
	timezones := []string{
		"America/New_York", "America/Los_Angeles", "America/Chicago",
		"Europe/London", "Europe/Berlin", "Asia/Tokyo", "Asia/Shanghai",
		"Australia/Sydney",
	}

	// Plausible os.release() values per platform (Node os.release()).
	osReleases := map[string][]string{
		"darwin": {"23.6.0", "24.1.0", "24.3.0", "24.4.0"},
		"linux":  {"6.5.0-44-generic", "6.8.0-41-generic", "5.15.0-122-generic"},
		"win32":  {"10.0.22631", "10.0.26100", "10.0.19045"},
	}
	releaseList := osReleases[platform]
	if len(releaseList) == 0 {
		releaseList = []string{"6.8.0-generic"}
	}
	osRelease := releaseList[rng.Intn(len(releaseList))]

	hostname := hostnames[rng.Intn(len(hostnames))]
	osUser := osUsers[rng.Intn(len(osUsers))]
	gitEmail := emails[rng.Intn(len(emails))]
	timezone := timezones[rng.Intn(len(timezones))]

	// Two stable MACs derived from the seed (not real addresses).
	mac1 := randomMAC(rng)
	mac2 := randomMAC(rng)

	// machineId: 32 hex chars, like /etc/machine-id on Linux. Seeded so
	// fingerprint and session context always share the same machine identity.
	machineID := randomHexToken(rng, 32)

	return ccSignals{
		MachineID:    machineID,
		MACAddresses: []string{mac1, mac2},
		OSUser:       osUser,
		Hostname:     hostname,
		GitEmail:     gitEmail,
		Platform:     platform,
		Arch:         arch,
		OSRelease:    osRelease,
		CPUModel:     cpuModel,
		CPUCount:     cpuCount,
		TotalMemGiB:  memGiB,
		IsContainer:  false,
		Timezone:     timezone,
	}
}

// signalsFor returns the stable per-apiKey raw signals, generating once.
func signalsFor(apiKey string) ccSignals {
	if apiKey == "" {
		apiKey = "anonymous"
	}
	scope := ccKeyScope(apiKey)
	if cached, ok := ccRawSignalPool.Load(scope); ok {
		if s, ok := cached.(ccSignals); ok {
			return s
		}
	}
	s := generateSignals(apiKey)
	ccRawSignalPool.Store(scope, s)
	return s
}

// CCFingerprintFor returns the stable fingerprint for the given apiKey,
// generating it on first use. The fingerprint is deterministic per apiKey.
func CCFingerprintFor(apiKey string) ccFingerprint {
	if apiKey == "" {
		apiKey = "anonymous"
	}
	scope := ccKeyScope(apiKey)
	if cached, ok := ccSignalPool.Load(scope); ok {
		if fp, ok := cached.(ccFingerprint); ok {
			return fp
		}
	}
	fp := buildFingerprint(signalsFor(apiKey))
	ccSignalPool.Store(scope, fp)
	return fp
}

// CCSessionIDFor returns a stable UUID-like session id for the given apiKey.
// The official CLI reuses one session id for the lifetime of a process session;
// we mirror that with a per-apiKey cached value so generate requests don't look
// like a new CLI process on every call.
func CCSessionIDFor(apiKey string) string {
	if apiKey == "" {
		apiKey = "anonymous"
	}
	scope := ccKeyScope(apiKey)
	if cached, ok := ccSessionIDPool.Load(scope); ok {
		if id, ok := cached.(string); ok && id != "" {
			return id
		}
	}
	// UUID v4 shape from seeded bytes (version/variant bits set correctly).
	rng := seededRand("cc-session-id", apiKey)
	var b [16]byte
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	ccSessionIDPool.Store(scope, id)
	return id
}

// ccSessionContext is the set of fake environment values that are stable per
// apiKey for the lifetime of the process. It mirrors the shape produced by
// the official CLI's getEnvironmentContext() / gatherRawSignals() but is
// populated from the seeded RNG instead of the local machine.
type ccSessionContext struct {
	WorkingDir    string
	Structure     []string
	IsGitRepo     bool
	CurrentBranch string
	MainBranch    string
	GitStatus     string
	RecentCommits []string
	ProjectSlug   string
	Environment   string
}

// CCSessionContextFor returns the stable per-apiKey session context.
// Uses the same raw signals as CCFingerprintFor so machine identity is coherent.
func CCSessionContextFor(apiKey string) *ccSessionContext {
	if apiKey == "" {
		apiKey = "anonymous"
	}
	scope := ccKeyScope(apiKey)
	if cached, ok := ccSessionPool.Load(scope); ok {
		if sc, ok := cached.(*ccSessionContext); ok {
			return sc
		}
	}
	sc := buildSessionContext(signalsFor(apiKey))
	ccSessionPool.Store(scope, sc)
	return sc
}

func buildSessionContext(signals ccSignals) *ccSessionContext {
	rng := seededRand("cc-session", signals.MachineID, signals.Hostname, signals.GitEmail)

	projectNames := []string{
		"my-app", "workspace", "monorepo", "platform", "service",
		"frontend", "backend", "api", "web", "tools",
	}
	projectName := projectNames[rng.Intn(len(projectNames))]

	dirPrefixes := map[string]string{
		"darwin": "/Users/" + signals.OSUser + "/code",
		"linux":  "/home/" + signals.OSUser + "/code",
		"win32":  "C:/Users/" + signals.OSUser + "/code",
	}
	prefix := dirPrefixes[signals.Platform]
	if prefix == "" {
		prefix = "/workspace"
	}
	workingDir := prefix + "/" + projectName

	dirs := []string{"src", "lib", "components", "pages", "public", "tests", "scripts", "docs"}
	structure := make([]string, 0, len(dirs))
	for _, d := range dirs {
		structure = append(structure, d)
	}

	branches := []string{
		"main", "develop", "feat/new-ui", "feat/api-cache", "fix/login",
		"refactor/types", "chore/deps", "release/v1.2.0",
	}
	currentBranch := branches[rng.Intn(len(branches))]
	mainBranch := "main"
	if rng.Intn(5) == 0 {
		mainBranch = "master"
	}

	statuses := []string{
		"", // clean tree, matches "Working tree clean" lookup
		"M 1",
		"M 2, A 1",
		"M 1, ?? 3",
		"A 2",
		"?? 4",
	}
	gitStatus := "Working tree clean"
	if raw := statuses[rng.Intn(len(statuses))]; raw != "" {
		gitStatus = raw
	}

	commits := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("commit-%s-%d-%d", signals.MachineID, i, rng.Int63())))
		commits = append(commits, hex.EncodeToString(h[:7]))
	}

	env := fmt.Sprintf("%s-%s, Node.js v%s", signals.Platform, signals.Arch, "22.11.0")

	return &ccSessionContext{
		WorkingDir:    workingDir,
		Structure:     structure,
		IsGitRepo:     true,
		CurrentBranch: currentBranch,
		MainBranch:    mainBranch,
		GitStatus:     gitStatus,
		RecentCommits: commits,
		ProjectSlug:   projectName,
		Environment:   env,
	}
}

// RecordFingerprintIfNeeded posts the device fingerprint to
// /alpha/fingerprint/record once per (baseURL, apiKey) pair. The official CLI
// performs this in the background at startup; we replicate that pattern with
// best-effort semantics — failures are swallowed and never block chat calls.
//
// The fingerprint is recorded before the first /alpha/generate call. The
// chat goroutine fires the request and proceeds without waiting; the record
// happens asynchronously so chat latency is unaffected.
func RecordFingerprintIfNeeded(baseURL, apiKey string) {
	if apiKey == "" || baseURL == "" {
		return
	}
	scope := ccKeyScope(baseURL + "|" + apiKey)
	if _, already := ccRecordedKeys.LoadOrStore(scope, struct{}{}); already {
		return
	}

	fp := CCFingerprintFor(apiKey)
	body, err := json.Marshal(fp)
	if err != nil {
		return
	}

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(bgCtx, http.MethodPost,
			strings.TrimRight(baseURL, "/")+"/alpha/fingerprint/record", bytes.NewReader(body))
		if err != nil {
			return
		}
		applyFingerprintHeaders(req, apiKey)

		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}()
}

// applyFingerprintHeaders sets the same headers the official CLI attaches to
// /alpha/fingerprint/record (recordCliFingerprint in command-code@1.44.0).
// Keys are stored lowercase to match Node fetch/undici over HTTP/1.1.
func applyFingerprintHeaders(req *http.Request, apiKey string) {
	setLower := func(key, value string) {
		req.Header.Del(key)
		req.Header[key] = []string{value}
	}
	setLower("content-type", "application/json")
	setLower("accept", "*/*")
	setLower("accept-language", "*")
	setLower("accept-encoding", "gzip, deflate, br")
	setLower("authorization", "Bearer "+apiKey)
	setLower("x-cli-environment", "production")
	setLower("x-command-code-version", CCCLIVersion)
	setLower("User-Agent", "cli")
}

// EnsureFingerprintRecorded is a synchronous variant that blocks on the
// fingerprint post. Used by tests that need to observe the recorded body
// before the chat request fires.
func EnsureFingerprintRecorded(ctx context.Context, baseURL, apiKey string) error {
	if apiKey == "" || baseURL == "" {
		return nil
	}
	scope := ccKeyScope(baseURL + "|" + apiKey)
	if _, already := ccRecordedKeys.LoadOrStore(scope, struct{}{}); already {
		return nil
	}
	fp := CCFingerprintFor(apiKey)
	body, err := json.Marshal(fp)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/alpha/fingerprint/record", bytes.NewReader(body))
	if err != nil {
		return err
	}
	applyFingerprintHeaders(req, apiKey)

	client := &http.Client{Timeout: 4 * time.Second}
	resp, doErr := client.Do(req)
	if doErr != nil {
		return doErr
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("commandcode: fingerprint record status=%d", resp.StatusCode)
	}
	return nil
}

// ResetFingerprintCache clears the recorded-keys and per-key caches. Used by
// tests.
func ResetFingerprintCache() {
	ccRecordedKeys = sync.Map{}
	ccRawSignalPool = sync.Map{}
	ccSignalPool = sync.Map{}
	ccSessionPool = sync.Map{}
	ccSessionIDPool = sync.Map{}
}
