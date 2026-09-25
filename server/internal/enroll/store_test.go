package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dialler/server/internal/secrets"
)

// A pre-9b devices.json (a bare map with "label") is read, its label
// becomes the description, and the first save rewrites it as schema 1.
func TestOpenMigratesTheLegacyShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	legacy := `{"dev-a":{"user":"201","label":"Matt's iPhone","token_hash":"abc","issued_at":"2026-09-01T00:00:00Z","revoked":false,"ssids":["Office"],"config_version":2}}`
	_ = os.WriteFile(path, []byte(legacy), 0o600)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := s.Device("dev-a")
	if !ok || d.Description != "Matt's iPhone" || d.User != "201" || !d.Enrolled || d.Config == nil || d.Config.Version != 2 {
		t.Fatalf("legacy record: %+v", d)
	}
	if _, err := s.SetDescription("dev-a", "Matt's iPhone (kept)"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var f struct {
		Schema  int                        `json:"schema"`
		Devices map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Schema != 1 || len(f.Devices) != 1 {
		t.Fatalf("rewritten file: %s (%v)", raw, err)
	}
	var rec map[string]any
	_ = json.Unmarshal(f.Devices["dev-a"], &rec)
	if _, hasLabel := rec["label"]; hasLabel || rec["description"] != "Matt's iPhone (kept)" {
		t.Fatalf("label must not be written back: %v", rec)
	}
	if _, ok := rec["updated_at"]; !ok {
		t.Fatalf("updated_at missing after a write: %v", rec)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := asJSON(t, s.Devices()), asJSON(t, s2.Devices()); a != b {
		t.Fatalf("round trip:\n%s\n%s", a, b)
	}
}

// asJSON compares views by their wire form: reflect.DeepEqual would trip
// on a time.Time's monotonic reading, which a file cannot carry.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	_ = os.WriteFile(path, []byte(`{"schema":2,"devices":{}}`), 0o600)
	if _, err := Open(path); err == nil {
		t.Fatal("a newer schema must be refused, not misread")
	}
	_ = os.WriteFile(path, []byte(`{"schema":1,"devices":{}}`), 0o600)
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndIssueSemantics(t *testing.T) {
	s, _ := Open("")
	if _, err := s.Create("dev-a", "201", "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("dev-a", "202", "B"); !errors.Is(err, ErrExists) {
		t.Fatalf("existing id: %v", err)
	}
	if _, err := s.Create("dev-b", "201", "B"); !errors.Is(err, ErrUserTaken) {
		t.Fatalf("taken user: %v", err)
	}
	if _, err := s.IssueToken("dev-c", "201", "tok_1234567890123456"); !errors.Is(err, ErrUserTaken) {
		t.Fatalf("taken user via issue: %v", err)
	}
	if _, err := s.IssueToken("dev-a", "999", "tok_1234567890123456"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("user change via issue: %v", err)
	}
	// Re-issuing keeps everything but the credential.
	s.Secrets, _ = secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	s.SetConfig("dev-a", []string{"Office"})
	s.SetPBXLine("dev-a", "", "line201", "secret")
	code, _, _ := s.MintCode("dev-a")
	s.Revoke("dev-a")
	tok, err := s.IssueToken("dev-a", "201", "tok_1234567890123456")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.Device("dev-a")
	if d.Description != "A" || d.Config == nil || d.Config.Version != 1 || d.PBXLine == nil || !d.CodePending || !d.Revoked || !d.Enrolled {
		t.Fatalf("re-issue disturbed the record: %+v", d)
	}
	if ok, _ := s.Authenticate(context.Background(), "dev-a", tok); ok {
		t.Fatal("a revoked device authenticated (re-issue does not un-revoke; a claim does)")
	}
	if c, err := s.Claim(code); err != nil || c.DeviceID != "dev-a" {
		t.Fatalf("the pending code survived re-issue and claims: %v", err)
	}
	if d, _ = s.Device("dev-a"); d.Revoked || d.CodePending {
		t.Fatalf("after the claim: %+v", d)
	}
}

func TestRevokedIsAnAuthenticationState(t *testing.T) {
	s, _ := Open("")
	s.Create("dev-a", "201", "A")
	s.Revoke("dev-a")
	if !s.Exists("dev-a") {
		t.Fatal("a revoked device still exists for the admin API")
	}
	if _, ok := s.UserFor("dev-a"); ok {
		t.Fatal("a revoked device must not exist for the call path")
	}
	if _, changed, err := s.SetConfig("dev-a", []string{"Office"}); err != nil || !changed {
		t.Fatalf("settings on a revoked device: %v", err)
	}
	if ok, _ := s.SetDescription("dev-a", "B"); !ok {
		t.Fatal("description on a revoked device")
	}
	if ok, err := s.Revoke("dev-a"); !ok || err != nil {
		t.Fatalf("re-revoke is idempotent: %v %v", ok, err)
	}
	if ok, _ := s.Purge("dev-a"); !ok || s.Exists("dev-a") {
		t.Fatal("purge")
	}
	if ok, _ := s.Purge("dev-a"); ok {
		t.Fatal("purge of an unknown device")
	}
	// The user is free again at once.
	if _, err := s.Create("dev-b", "201", ""); err != nil {
		t.Fatalf("user after purge: %v", err)
	}
}

func TestCancelCodeAndUpdatedAt(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	s, _ := Open("")
	s.now = func() time.Time { return now }
	s.Create("dev-a", "201", "")
	if s.UpdatedAt("dev-a") != now {
		t.Fatal("updated_at on create")
	}
	now = now.Add(time.Minute)
	s.MintCode("dev-a")
	if d, _ := s.Device("dev-a"); !d.CodePending || d.CodeExpiresAt != now.Add(CodeTTL) || d.UpdatedAt != now {
		t.Fatalf("after mint: %+v", d)
	}
	now = now.Add(time.Minute)
	if ok, err := s.CancelCode("dev-a"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if d, _ := s.Device("dev-a"); d.CodePending || d.UpdatedAt != now {
		t.Fatalf("after cancel: %+v", d)
	}
	now = now.Add(time.Minute)
	s.CancelCode("dev-a") // nothing pending: no write, updated_at stays
	if s.UpdatedAt("dev-a") != now.Add(-time.Minute) {
		t.Fatal("a no-op cancel moved updated_at")
	}
	if ok, _ := s.CancelCode("nope"); ok {
		t.Fatal("unknown device")
	}
}

// A failed persist leaves memory exactly as it was (ADMIN-API.md §4.4:
// 500 store, entry unchanged).
func TestFailedPersistRestoresMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	s, _ := Open(path)
	s.Create("dev-a", "201", "A")
	// Make the directory unwritable for the temp file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Getuid() == 0 {
		t.Skip("root ignores permissions")
	}
	if _, err := s.Create("dev-b", "202", "B"); err == nil {
		t.Fatal("the write should have failed")
	}
	if s.Exists("dev-b") {
		t.Fatal("dev-b exists in memory after a failed persist")
	}
	if _, err := s.SetDescription("dev-a", "changed"); err == nil {
		t.Fatal("the write should have failed")
	}
	if d, _ := s.Device("dev-a"); d.Description != "A" {
		t.Fatalf("dev-a changed in memory after a failed persist: %+v", d)
	}
}

// Round trip property: after a random walk of operations, the file
// re-opens to exactly the in-memory view (ADMIN-API.md §6.2).
func TestRoundTripProperty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s, _ := Open(path)
	s.Realm = "dialler"
	s.Secrets, _ = secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	rng := rand.New(rand.NewSource(1))
	ids := []string{"dev-a", "dev-b", "dev-c", "dev-d"}
	for i := 0; i < 300; i++ {
		id := ids[rng.Intn(len(ids))]
		switch rng.Intn(9) {
		case 0:
			s.Create(id, "20"+id[4:], "desc "+id)
		case 1:
			s.IssueToken(id, "20"+id[4:], "tok_fixed_"+id+"_0123456789")
		case 2:
			s.SetConfig(id, []string{"Office", id})
		case 3:
			s.SetPBXLine(id, "", "line"+id, "s3cret")
		case 4:
			s.MintCode(id)
		case 5:
			s.Revoke(id)
		case 6:
			s.SetDescription(id, "renamed")
		case 7:
			s.DeletePBXLine(id)
		case 8:
			if rng.Intn(10) == 0 {
				s.Purge(id)
			}
		}
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	again.Secrets = s.Secrets
	if a, b := asJSON(t, s.Devices()), asJSON(t, again.Devices()); a != b {
		t.Fatalf("round trip differs:\n%s\n%s", a, b)
	}
	ca, _ := s.PBXCredentials()
	cb, _ := again.PBXCredentials()
	sort.Slice(ca, func(i, j int) bool { return ca[i].DeviceID < ca[j].DeviceID })
	sort.Slice(cb, func(i, j int) bool { return cb[i].DeviceID < cb[j].DeviceID })
	if !reflect.DeepEqual(ca, cb) {
		t.Fatal("line credentials differ after round trip")
	}
}

// The call path's credential read never waits on disk (ADMIN-API.md
// §4.7): under a continuous stream of writes to a real file, the digest
// lookup stays under a millisecond at p99.
func TestDigestReadNeverWaitsOnDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	path := filepath.Join(t.TempDir(), "devices.json")
	s, _ := Open(path)
	s.Realm = "dialler"
	for i := 0; i < 200; i++ {
		s.IssueToken("dev-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "u"+string(rune('a'+i%26))+string(rune('a'+i/26)), "tok_fixed_0123456789_"+string(rune('a'+i%26)))
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			s.SetConfig("dev-aa", []string{"Office", string(rune('a' + i%26))})
		}
	}()
	time.Sleep(20 * time.Millisecond)
	const n = 3000
	lat := make([]time.Duration, n)
	for i := range lat {
		t0 := time.Now()
		s.DigestSecret("dev-ba")
		lat[i] = time.Since(t0)
		time.Sleep(50 * time.Microsecond)
	}
	stop.Store(true)
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p99 := lat[n*99/100]
	t.Logf("digest read under continuous writes: p50 %v p99 %v max %v", lat[n/2], p99, lat[n-1])
	if p99 > time.Millisecond {
		t.Fatalf("p99 %v over the 1 ms bound: the read is waiting on disk", p99)
	}
}
