// Package passwd hashes and verifies passwords with Argon2id.
//
// Hashes are stored in the standard PHC string format, so the parameters travel
// with the hash and can be raised later without invalidating existing hashes.
package passwd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters used for new hashes.
type Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams targets roughly 64 MiB and a few hundred milliseconds, which is
// comfortably strong for a login that happens once every 90 days and cheap
// enough for the smallest Fly machine.
var DefaultParams = Params{
	Memory:      64 * 1024,
	Iterations:  3,
	Parallelism: 2,
	SaltLength:  16,
	KeyLength:   32,
}

var (
	ErrMismatch      = errors.New("passwd: password does not match")
	ErrInvalidHash   = errors.New("passwd: malformed hash")
	ErrIncompatible  = errors.New("passwd: incompatible hash algorithm")
	ErrEmptyPassword = errors.New("passwd: password must not be empty")
)

// Hash derives a PHC-formatted Argon2id hash of password.
func Hash(password string) (string, error) {
	return HashWith(password, DefaultParams)
}

// HashWith derives a hash using explicit parameters.
func HashWith(password string, p Params) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("passwd: read salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify reports whether password matches encoded.
// It returns ErrMismatch on a wrong password and a different error if the
// stored hash itself is unusable, so the two cases can be told apart in logs.
func Verify(password, encoded string) error {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return err
	}

	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

func decode(encoded string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, ErrIncompatible
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, ErrIncompatible
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return p, nil, nil, ErrInvalidHash
	}

	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	return p, salt, key, nil
}
