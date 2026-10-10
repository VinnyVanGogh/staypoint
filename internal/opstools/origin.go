package opstools

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Board review #2 H1: an agent could start its own `staypoint mcp` with a
// fake HOME (so a fake config.toml) or with STAYPOINT_TASK_ID unset, and the
// ops tools ran under whatever that process was told. The harness now hands
// each run a token, HMAC(ops_key, task ID), and the MCP server runs ops tools
// only for a task whose token checks out against the key in the real data
// dir. An agent can read its own run's token, but cannot mint one for
// another task without the key, which lives under ~/.staypoint (a sensitive
// path to the hook).

// RunTokenEnv carries the run token from the harness to the MCP server. Its
// name avoids TOKEN/SECRET/KEY so env sanitizers that drop secret-looking
// names (security.SanitizeEnv) pass it through to the MCP server.
const RunTokenEnv = "STAYPOINT_RUN_MAC"

const opsKeyFile = "ops_key"

// RunToken is the token the harness gives taskID's run, creating the key in
// dataDir on first use.
func RunToken(dataDir, taskID string) (string, error) {
	key, err := opsKey(dataDir, true)
	if err != nil {
		return "", err
	}
	return runMAC(key, taskID), nil
}

// VerifyRunToken reports whether token is the harness's token for taskID.
// It never creates the key: no key means no run was ever issued one.
func VerifyRunToken(dataDir, taskID, token string) error {
	if strings.TrimSpace(taskID) == "" {
		return errors.New("no STAYPOINT_TASK_ID: ops tools run only inside a StayPoint run")
	}
	if token == "" {
		return fmt.Errorf("no %s: ops tools run only in an MCP server the StayPoint harness started", RunTokenEnv)
	}
	key, err := opsKey(dataDir, false)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(runMAC(key, taskID)), []byte(token)) {
		return fmt.Errorf("%s does not match task %s", RunTokenEnv, taskID)
	}
	return nil
}

func runMAC(key []byte, taskID string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("staypoint-ops-run\x00" + taskID))
	return hex.EncodeToString(m.Sum(nil))
}

// opsKey reads dataDir/ops_key, creating it (0600, 32 random bytes) when
// create is set. A key anyone but the owner can read is refused.
func opsKey(dataDir string, create bool) ([]byte, error) {
	if dataDir == "" || !filepath.IsAbs(dataDir) {
		return nil, errors.New("ops key: data dir is not an absolute path")
	}
	p := filepath.Join(dataDir, opsKeyFile)
	if create {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("ops key: %w", err)
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("ops key: %w", err)
		}
		// Written whole to a temp file, then linked into place, so a
		// concurrent reader never sees a half-written key; losing the race
		// to another run keeps that run's key.
		tmp, err := os.CreateTemp(dataDir, ".ops_key-*")
		if err != nil {
			return nil, fmt.Errorf("ops key: %w", err)
		}
		_, werr := tmp.Write([]byte(hex.EncodeToString(buf)))
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			if lerr := os.Link(tmp.Name(), p); lerr != nil && !errors.Is(lerr, os.ErrExist) {
				werr = lerr
			}
		}
		_ = os.Remove(tmp.Name())
		if werr != nil {
			return nil, fmt.Errorf("ops key: %w", werr)
		}
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("ops key: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("ops key %s must be a regular file readable only by its owner", p)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("ops key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("ops key %s is not a 32-byte hex key", p)
	}
	return key, nil
}
