// Command dialler-licence is the vendor's tool for product licences (SPEC
// §4.9): it makes the vendor key pair once, issues a licence for one
// customer's server, and inspects a licence token.
//
//	dialler-licence keygen -out DIR
//	dialler-licence issue -key DIR/vendor.key -customer "Example Ltd" \
//	    -install-id <id from the customer's admin Licence page> \
//	    -seats 25 -valid-until 2027-12-31 [-id lic_XXXXXXXX]
//	dialler-licence inspect [-pub DIR/vendor.pub] TOKEN
//
// The private key never leaves the vendor; the public half is pasted into
// server/internal/licence/vendorkey.go and the app's verifier, which keygen
// prints ready to copy. It runs offline; nothing here talks to a network.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dialler/server/internal/licence"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process: tests drive it with arguments and read
// what it printed.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: dialler-licence keygen|issue|inspect ...")
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = keygen(args[1:], stdout, stderr)
	case "issue":
		err = issue(args[1:], stdout, stderr)
	case "inspect":
		err = inspect(args[1:], stdout, stderr)
	default:
		err = fmt.Errorf("unknown command %q (keygen, issue, inspect)", args[0])
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 2
		}
		fmt.Fprintln(stderr, "dialler-licence:", err)
		return 1
	}
	return 0
}

// keygen makes the vendor key pair: vendor.key (the 32-byte seed, base64,
// mode 0600) and vendor.pub (the 32-byte public key, base64), and prints
// the literals to paste into the server and the app.
func keygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", ".", "directory to write vendor.key and vendor.pub into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keyPath, pubPath := filepath.Join(*out, "vendor.key"), filepath.Join(*out, "vendor.pub")
	for _, p := range []string{keyPath, pubPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists; a second key pair would strand every licence issued under the first, so remove it deliberately first", p)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	if err := os.WriteFile(keyPath, []byte(seed+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, []byte(pubB64+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (keep offline, never commit) and %s\n\n", keyPath, pubPath)
	fmt.Fprintf(stdout, "server/internal/licence/vendorkey.go:\n\tconst vendorPublicKeyBase64 = %q\n\n", pubB64)
	fmt.Fprintf(stdout, "ios (DiallerCore Licence.swift):\n\tstatic let vendorPublicKey = Data(base64Encoded: %q)!\n", pubB64)
	return nil
}

// loadKey reads vendor.key.
func loadKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a vendor key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// loadPub reads vendor.pub.
func loadPub(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s is not a vendor public key", path)
	}
	return pub, nil
}

// issue signs one licence and prints the token.
func issue(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "vendor.key", "the vendor private key from keygen")
	customer := fs.String("customer", "", "the licensee, shown on their admin pages")
	install := fs.String("install-id", "", "the install id from the customer's admin Licence page")
	seats := fs.Int("seats", 0, "how many app devices may be enrolled")
	until := fs.String("valid-until", "", "last day of validity, YYYY-MM-DD (expires at the start of the next day, UTC)")
	id := fs.String("id", "", "licence id (default: lic_ + 8 random characters)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *customer == "":
		return errors.New("-customer is required")
	case *install == "":
		return errors.New("-install-id is required")
	case *seats <= 0:
		return errors.New("-seats must be at least 1")
	case *until == "":
		return errors.New("-valid-until is required")
	}
	day, err := time.Parse("2006-01-02", *until)
	if err != nil {
		return fmt.Errorf("-valid-until: %w", err)
	}
	priv, err := loadKey(*key)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = "lic_" + randomID(8)
	}
	now := time.Now().UTC().Truncate(time.Second)
	l := licence.Licence{
		V:          1,
		ID:         *id,
		Customer:   *customer,
		InstallID:  *install,
		Seats:      *seats,
		IssuedAt:   now,
		ValidUntil: day.Add(24 * time.Hour),
	}
	tok, err := licence.Sign(priv, l)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, tok)
	fmt.Fprintf(stderr, "issued %s to %q for install id %s: %d seats until %s\n", l.ID, l.Customer, l.InstallID, l.Seats, l.ValidUntil.Format("2006-01-02 15:04 UTC"))
	return nil
}

// crockford is the alphabet the enrolment codes use too: no I, L, O, U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = crockford[int(b[i])%len(crockford)]
	}
	return string(b)
}

// inspect prints a token's payload and whether it verifies, against the
// compiled-in vendor key or the one in -pub.
func inspect(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pubPath := fs.String("pub", "", "verify with this vendor.pub instead of the key compiled in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("inspect takes one token")
	}
	if *pubPath != "" {
		pub, err := loadPub(*pubPath)
		if err != nil {
			return err
		}
		licence.PublicKey = pub
	}
	l, err := licence.Parse(strings.TrimSpace(fs.Arg(0)))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "id          %s\ncustomer    %s\ninstall id  %s\nseats       %d\nissued      %s\nvalid until %s\nstate       %s (%d days left)\nsignature   verified\n",
		l.ID, l.Customer, l.InstallID, l.Seats, l.IssuedAt.UTC().Format(time.RFC3339), l.ValidUntil.UTC().Format(time.RFC3339), l.StateAt(time.Now()), l.DaysLeft(time.Now()))
	return nil
}
