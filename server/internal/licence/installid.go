package licence

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// installIDGrammar is what an install id looks like: the server mints 32 hex
// characters; a hand-copied one with surrounding whitespace is tolerated,
// anything else is a damaged file, not a reason to mint a new identity.
var installIDGrammar = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// LoadOrCreateInstallID returns the installation's id, minting one on the
// first start and keeping it for ever after. Follows secrets.OpenKey: the
// file is written 0600 in a directory created 0700. A file that is present
// but malformed is an error, since silently replacing it would quietly
// invalidate the licence bound to the old one.
func LoadOrCreateInstallID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("licence: write %s: %w", path, err)
		}
		return id, nil
	case err != nil:
		return "", fmt.Errorf("licence: read %s: %w", path, err)
	}
	id := strings.TrimSpace(string(raw))
	if !installIDGrammar.MatchString(id) {
		return "", fmt.Errorf("licence: %s does not hold an install id (8–64 letters, digits, - or _); restore it from backup, or remove it to start a new installation that will need a new licence", path)
	}
	return id, nil
}
