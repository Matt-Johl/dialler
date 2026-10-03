package enroll

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dialler/server/internal/admin"
	"dialler/server/internal/secrets"
)

// seatStore is a memory store on a hand-driven clock with the given seat
// count, with a secrets box so lines can be set.
func seatStore(t *testing.T, seats int) (*Store, *time.Time) {
	t.Helper()
	s, _ := Open("")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	box, err := secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Secrets = box
	s.Realm = "dialler"
	s.SetSeats(seats)
	return s, &now
}

func issue(t *testing.T, s *Store, id, user string) {
	t.Helper()
	if _, err := s.IssueToken(id, user, "fixture-token-"+id); err != nil {
		t.Fatalf("issue %s: %v", id, err)
	}
}

func licensedIDs(s *Store) []string {
	var out []string
	for _, d := range s.Devices() {
		if d.Licensed {
			out = append(out, d.DeviceID)
		}
	}
	return out
}

func TestSeatsDefaultToUnlimited(t *testing.T) {
	s, _ := Open("")
	issue(t, s, "dev-a", "201")
	issue(t, s, "dev-b", "202")
	used, total := s.Seats()
	if used != 2 || total != NoSeatLimit {
		t.Fatalf("a store nobody gave a seat count must count but not limit: used=%d total=%d", used, total)
	}
	if !s.Licensed("dev-a") || !s.Licensed("dev-b") {
		t.Fatal("unlimited: every holder is licensed")
	}
}

func TestSeatsAreTakenAtEnrolmentAndFreedByRevoke(t *testing.T) {
	s, _ := seatStore(t, 1)
	issue(t, s, "dev-a", "201")
	if _, err := s.IssueToken("dev-b", "202", "fixture-token-dev-b"); !errors.Is(err, ErrNoSeats) {
		t.Fatalf("second device on a one-seat licence: err = %v, want ErrNoSeats", err)
	}
	if s.Exists("dev-b") {
		t.Fatal("a refused issue left a record behind")
	}
	// Re-issuing the holder's credential is not a second seat.
	issue(t, s, "dev-a", "201")
	if used, _ := s.Seats(); used != 1 {
		t.Fatalf("re-issue counted a seat: used=%d", used)
	}
	// Creating a record without a credential, and minting its code, is
	// always allowed: that is staging, not enrolment.
	if _, err := s.Create("dev-b", "202", "staged"); err != nil {
		t.Fatalf("create at full: %v", err)
	}
	code, _, err := s.MintCode("dev-b")
	if err != nil {
		t.Fatalf("mint at full: %v", err)
	}
	// Claiming it is refused, and the code is not spent.
	if _, err := s.Claim(code); !errors.Is(err, ErrNoSeats) {
		t.Fatalf("claim at full: err = %v, want ErrNoSeats", err)
	}
	if d, _ := s.Device("dev-b"); d.Enrolled || !d.CodePending {
		t.Fatalf("a refused claim must leave the device un-enrolled with its code pending: %+v", d)
	}
	// Revoking the holder frees the seat; the same code now claims.
	if _, err := s.Revoke("dev-a"); err != nil {
		t.Fatal(err)
	}
	if used, _ := s.Seats(); used != 0 {
		t.Fatalf("revoke must free the seat: used=%d", used)
	}
	c, err := s.Claim(code)
	if err != nil || c.DeviceID != "dev-b" {
		t.Fatalf("claim after a seat freed: %v %+v", err, c)
	}
	if !s.Licensed("dev-b") || s.Licensed("dev-a") {
		t.Fatal("dev-b holds the seat now, dev-a is revoked")
	}
	// A revoked device's code with no seat free is refused and it stays
	// revoked.
	codeA, _, _ := s.MintCode("dev-a")
	if _, err := s.Claim(codeA); !errors.Is(err, ErrNoSeats) {
		t.Fatalf("revoked device's claim at full: %v", err)
	}
	if d, _ := s.Device("dev-a"); !d.Revoked {
		t.Fatal("a refused claim un-revoked the device")
	}
	// A holder re-claiming (credential rotation) needs no second seat.
	codeB, _, _ := s.MintCode("dev-b")
	if _, err := s.Claim(codeB); err != nil {
		t.Fatalf("a holder's re-claim: %v", err)
	}
	if used, _ := s.Seats(); used != 1 {
		t.Fatalf("re-claim counted a seat: used=%d", used)
	}
}

// When there are more holders than seats, the first by seat_since keep
// theirs: shrinking suspends exactly the newest, growing reinstates them
// in order, and a re-claim does not send a device to the back.
func TestOverflowOrderIsStableAcrossReClaim(t *testing.T) {
	s, now := seatStore(t, 3)
	var changes []SeatChange
	s.OnSeats(func(c SeatChange) { changes = append(changes, c) })
	for _, id := range []string{"dev-a", "dev-b", "dev-c"} {
		*now = now.Add(time.Minute)
		issue(t, s, id, "2"+id[len(id)-2:])
	}
	changes = nil
	s.SetSeats(2)
	if got := licensedIDs(s); strings.Join(got, ",") != "dev-a,dev-b" {
		t.Fatalf("shrink to 2 must keep the two oldest, got %v", got)
	}
	if len(changes) != 1 || strings.Join(changes[0].Lost, ",") != "dev-c" || len(changes[0].Gained) != 0 || changes[0].Used != 3 || changes[0].Seats != 2 {
		t.Fatalf("shrink change = %+v", changes)
	}
	if st := s.SeatState("dev-c"); st != SeatUnlicensed {
		t.Fatalf("dev-c state %q, want unlicensed", st)
	}
	// dev-b rotates its credential a day later: still second in line.
	*now = now.Add(24 * time.Hour)
	code, _, _ := s.MintCode("dev-b")
	if _, err := s.Claim(code); err != nil {
		t.Fatal(err)
	}
	if got := licensedIDs(s); strings.Join(got, ",") != "dev-a,dev-b" {
		t.Fatalf("after dev-b re-claimed: licensed %v, want a and b", got)
	}
	changes = nil
	s.SetSeats(3)
	if got := licensedIDs(s); strings.Join(got, ",") != "dev-a,dev-b,dev-c" {
		t.Fatalf("grow to 3: %v", got)
	}
	if len(changes) != 1 || strings.Join(changes[0].Gained, ",") != "dev-c" || len(changes[0].Lost) != 0 {
		t.Fatalf("grow change = %+v", changes)
	}
	// Setting the same count again changes nothing and says nothing.
	changes = nil
	s.SetSeats(3)
	if len(changes) != 0 {
		t.Fatalf("an unchanged seat count fired %+v", changes)
	}
	// Zero seats (no licence, or expired) suspends everyone; the records
	// and their credentials are untouched.
	s.SetSeats(0)
	if got := licensedIDs(s); len(got) != 0 {
		t.Fatalf("zero seats: licensed %v", got)
	}
	if ok, _ := s.Authenticate(nil, "dev-a", "fixture-token-dev-a"); !ok {
		t.Fatal("Authenticate is the credential check and must still pass for a suspended device")
	}
}

// A revoked-then-reclaimed device re-enters the queue at the back, since
// it gave its seat up; a device that merely rotated its credential does
// not (previous test).
func TestARevokedDeviceComingBackJoinsTheBack(t *testing.T) {
	s, now := seatStore(t, 3)
	for _, id := range []string{"dev-a", "dev-b", "dev-c"} {
		*now = now.Add(time.Minute)
		issue(t, s, id, "2"+id[len(id)-2:])
	}
	s.Revoke("dev-a")
	*now = now.Add(time.Hour)
	code, _, _ := s.MintCode("dev-a")
	if _, err := s.Claim(code); err != nil {
		t.Fatal(err)
	}
	s.SetSeats(2)
	if got := licensedIDs(s); strings.Join(got, ",") != "dev-b,dev-c" {
		t.Fatalf("dev-a came back last and must be the one suspended: %v", got)
	}
}

func TestEveryDoorRefusesAnUnlicensedDevice(t *testing.T) {
	// Three holders on three seats, then the licence shrinks to two: dev-c,
	// the newest, is the one beyond the line.
	s, now := seatStore(t, 3)
	for _, id := range []string{"dev-a", "dev-b", "dev-c"} {
		*now = now.Add(time.Minute)
		issue(t, s, id, "2"+id[len(id)-2:])
		if _, err := s.SetPBXLine(id, "", "line-"+id, "secret"); err != nil {
			t.Fatal(err)
		}
	}
	s.SetSeats(2)
	// dev-d: provisioned with a line, not yet enrolled. Its line is still
	// registered, as today, so it answers the moment the phone arrives.
	if _, err := s.Create("dev-d", "204", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPBXLine("dev-d", "", "line-dev-d", "secret"); err != nil {
		t.Fatal(err)
	}

	// SIP: the digest secret is withheld.
	if _, _, ok := s.DigestSecret("dev-c"); ok {
		t.Error("DigestSecret must refuse an unlicensed device")
	}
	if _, _, ok := s.DigestSecret("dev-b"); !ok {
		t.Error("DigestSecret must still serve a licensed device")
	}
	// The user is still known: the hooks need it to drop the line.
	if u, ok := s.UserFor("dev-c"); !ok || u != "2-c" {
		t.Errorf("UserFor(dev-c) = %q %v", u, ok)
	}
	// Lines: unlicensed holders are left out, the un-enrolled stays in.
	creds, err := s.PBXCredentials()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range creds {
		ids = append(ids, c.DeviceID)
	}
	if strings.Join(ids, ",") != "dev-a,dev-b,dev-d" {
		t.Errorf("PBXCredentials = %v, want a, b and the un-enrolled d", ids)
	}
	// The device HTTP API.
	h := s.DeviceAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	call := func(id string) int {
		req := httptest.NewRequest("GET", "/v1/directory", nil)
		req.Header.Set("X-Device-ID", id)
		req.Header.Set("Authorization", "Bearer fixture-token-"+id)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if call("dev-b") != 204 || call("dev-c") != 401 {
		t.Errorf("DeviceAuth: licensed %d, unlicensed %d", call("dev-b"), call("dev-c"))
	}
	// The public view says which is which.
	if d, _ := s.Device("dev-c"); d.Licensed || !d.Enrolled {
		t.Errorf("view of an unlicensed holder: %+v", d)
	}
	if d, _ := s.Device("dev-d"); d.Licensed {
		t.Error("an un-enrolled device holds no seat and is not licensed")
	}
	if st := s.SeatState("dev-d"); st != SeatNone {
		t.Errorf("SeatState(dev-d) = %q, want none", st)
	}
	if st := s.SeatState("dev-a"); st != SeatHolder {
		t.Errorf("SeatState(dev-a) = %q, want holder", st)
	}
}

func TestOnSeatsFiresAfterTheSaveAndNotAfterAFailedOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetSeats(5)
	var changes []SeatChange
	s.OnSeats(func(c SeatChange) {
		// By the time the hook runs, the file holds the change.
		raw, _ := os.ReadFile(path)
		if !strings.Contains(string(raw), `"seat_since"`) {
			t.Error("OnSeats ran before the save")
		}
		changes = append(changes, c)
	})
	issue(t, s, "dev-a", "201")
	if len(changes) != 1 || strings.Join(changes[0].Gained, ",") != "dev-a" || changes[0].Used != 1 || changes[0].Seats != 5 {
		t.Fatalf("after one issue: %+v", changes)
	}
	// A write that changes no seat says nothing.
	changes = nil
	s.SetDescription("dev-a", "renamed")
	if len(changes) != 0 {
		t.Fatalf("a rename fired %+v", changes)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Getuid() == 0 {
		t.Skip("root ignores permissions")
	}
	if _, err := s.IssueToken("dev-b", "202", "fixture-token-dev-b"); err == nil {
		t.Fatal("the write should have failed")
	}
	if len(changes) != 0 || s.Licensed("dev-b") {
		t.Fatalf("a failed save leaked a seat: changes=%+v licensed=%v", changes, s.Licensed("dev-b"))
	}
	if used, _ := s.Seats(); used != 1 {
		t.Fatalf("used after a failed save = %d", used)
	}
}

// A devices.json written before seats existed has no seat_since: holders
// are ordered by issued_at, and the field appears on the next write.
func TestLegacyRecordsOrderByIssuedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	legacy := `{"schema":1,"devices":{
	  "dev-new":{"user":"203","token_hash":"h","ha1":"x","issued_at":"2026-09-30T10:00:00Z"},
	  "dev-old":{"user":"201","token_hash":"h","ha1":"x","issued_at":"2026-09-01T10:00:00Z"},
	  "dev-mid":{"user":"202","token_hash":"h","ha1":"x","issued_at":"2026-09-15T10:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetSeats(2)
	if got := licensedIDs(s); strings.Join(got, ",") != "dev-mid,dev-old" {
		t.Fatalf("legacy order: licensed %v, want the two oldest by issued_at", got)
	}
}

// Property: over a random walk of issue, claim, revoke, purge and seat
// changes, the licensed set is exactly the first N holders by seat order,
// and never more than the seats.
func TestSeatInvariantProperty(t *testing.T) {
	s, now := seatStore(t, 3)
	r := rand.New(rand.NewSource(7))
	ids := []string{"dev-a", "dev-b", "dev-c", "dev-d", "dev-e"}
	for i := range ids {
		if _, err := s.Create(ids[i], "20"+string(rune('1'+i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	for step := 0; step < 400; step++ {
		*now = now.Add(time.Second)
		id := ids[r.Intn(len(ids))]
		switch r.Intn(5) {
		case 0:
			_, err := s.IssueToken(id, "", "")
			if err != nil && !errors.Is(err, ErrNoSeats) && !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		case 1:
			if code, _, err := s.MintCode(id); err == nil {
				if _, err := s.Claim(code); err != nil && !errors.Is(err, ErrNoSeats) {
					t.Fatal(err)
				}
			}
		case 2:
			s.Revoke(id)
		case 3:
			s.Purge(id)
			s.Create(id, "20"+id[len(id)-1:], "")
		case 4:
			s.SetSeats(r.Intn(4))
		}
		used, total := s.Seats()
		lic := licensedIDs(s)
		if len(lic) > total {
			t.Fatalf("step %d: %d licensed with %d seats", step, len(lic), total)
		}
		if used >= total && len(lic) != total {
			t.Fatalf("step %d: %d holders, %d seats, but %d licensed", step, used, total, len(lic))
		}
		if used < total && len(lic) != used {
			t.Fatalf("step %d: %d holders fit in %d seats but %d licensed", step, used, total, len(lic))
		}
		// And they are the earliest: no licensed device is later than an
		// unlicensed one.
		var order []Device
		for _, d := range s.Devices() {
			if d.Enrolled && !d.Revoked {
				order = append(order, d)
			}
		}
		sort.SliceStable(order, func(i, j int) bool {
			a, b := order[i], order[j]
			if !a.SeatSince.Equal(b.SeatSince) {
				return a.SeatSince.Before(b.SeatSince)
			}
			return a.DeviceID < b.DeviceID
		})
		for i, d := range order {
			if d.Licensed != (i < total || total == NoSeatLimit) {
				t.Fatalf("step %d: %s at position %d licensed=%v with %d seats", step, d.DeviceID, i, d.Licensed, total)
			}
		}
	}
}

func TestEnrolHandlerRefusesWithNoSeats(t *testing.T) {
	s, now := seatStore(t, 1)
	issue(t, s, "dev-a", "201")
	h := NewEnrolHandler(s, EnrolInfo{SignalPort: 7443, SIPDomain: "dialler", Now: func() time.Time { return *now }}, Hooks{})
	do := func(body string) (*http.Response, []byte) {
		req := httptest.NewRequest("POST", "/v1/enrol", strings.NewReader(body))
		req.RemoteAddr = "10.0.0.9:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}
	s.Create("dev-b", "202", "")
	code, _, _ := s.MintCode("dev-b")
	resp, body := do(`{"code":"` + code + `"}`)
	if resp.StatusCode != 403 {
		t.Fatalf("claim with no seat: %d %s", resp.StatusCode, body)
	}
	var out map[string]string
	if json.Unmarshal(body, &out) != nil || out["error"] != "no_seats" || out["message"] == "" {
		t.Fatalf("403 body must be {error:no_seats, message}: %s", body)
	}
	s.Revoke("dev-a")
	if resp, body := do(`{"code":"` + code + `"}`); resp.StatusCode != 200 {
		t.Fatalf("the same code once a seat is free: %d %s", resp.StatusCode, body)
	}
}

func TestAdminCreateWithFixtureTokenRefusesWithNoSeats(t *testing.T) {
	s, _ := seatStore(t, 0)
	h := NewAdminHandler(s, "admin", Link{}, Hooks{})
	// Staging a device without a credential is allowed at zero seats.
	rec, _ := adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-a","user":"201"}`)
	if rec.Code != 201 {
		t.Fatalf("create without token at 0 seats: %d %s", rec.Code, rec.Body)
	}
	// A fixture token takes a seat, so it is refused, and no record is
	// left holding a credential.
	rec, body := adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-b","user":"202","token":"fixture-token-dev-b"}`)
	if rec.Code != 409 || body.Error != admin.CodeNoSeats {
		t.Fatalf("fixture token at 0 seats: %d %+v", rec.Code, body)
	}
	if s.Exists("dev-b") {
		t.Fatal("a refused fixture create left a record")
	}
	s.SetSeats(1)
	rec, _ = adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-b","user":"202","token":"fixture-token-dev-b"}`)
	if rec.Code != 201 {
		t.Fatalf("fixture token with a seat: %d %s", rec.Code, rec.Body)
	}
	// Re-posting the holder keeps its seat and is still 201.
	rec, _ = adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-b","user":"202","token":"fixture-token-dev-b"}`)
	if rec.Code != 201 {
		t.Fatalf("re-post of the holder: %d %s", rec.Code, rec.Body)
	}
	if used, _ := s.Seats(); used != 1 {
		t.Fatalf("re-post counted a seat: %d", used)
	}
}
