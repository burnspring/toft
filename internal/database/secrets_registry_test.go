package database

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Test 3. The registry is the specification, so something has to check that it
// is also the truth.
//
// `secrets.go`'s own comment states the failure this prevents: a ciphertext
// column that is not on `encryptedColumns` is one the rotate command silently
// skips and the startup report silently calls readable — *both lying with
// authority*, which is worse than having neither. Nothing enforced it, and the
// way a column gets missed is not carelessness but a feature that adds three
// at once and reconciles two.
//
// The net is the naming convention rather than the whole schema: a column whose
// name ends `_secret` or `_token` is a stored credential unless it is one of
// the exceptions named below, each of which says why. That leaves ciphertexts
// under other names — `env_vars.value` is one — outside the net, which is the
// honest limit of a convention check and the reason this test complements
// `secrets.go`'s comment rather than replacing it.
func TestEveryCiphertextColumnIsInTheRegistry(t *testing.T) {
	// Columns matching the convention that are *not* ciphertexts. Each entry
	// is a claim that has to stay true, which is why they are named one by one
	// rather than pattern-matched away.
	notCiphertext := map[string]string{
		"integration_connections.issue_tokens": "a boolean switch: whether apps may be " +
			"handed the connection's access token, which is minted per request and never stored",
	}

	registered := map[string]bool{}
	for _, c := range encryptedColumns {
		registered[c.table+"."+c.column] = true
	}

	snapshot, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("reading schema.sql: %v", err)
	}

	found := map[string]bool{}
	var missing []string
	for _, col := range credentialShapedColumns(string(snapshot)) {
		found[col] = true
		if notCiphertext[col] != "" || registered[col] {
			continue
		}
		missing = append(missing, col)
	}

	// An exception naming a column that no longer exists is a claim nobody is
	// checking any more, and the next column to take that name inherits it
	// silently.
	for col := range notCiphertext {
		if !found[col] {
			t.Errorf("notCiphertext names %s, which is not a column in the schema", col)
		}
	}
	// And the schema's own word for it: a table whose comment says it holds
	// something encrypted must have a column on the registry, whatever its
	// columns are called. credential_values.value was outside the naming net
	// and outside the registry (security review 2026-11-10, H-2).
	notEncryptedTable := map[string]string{
		"encryption_data_keys": "wrapped data keys, opened by the key-encryption key, not by the data key rotate-key moves",
	}
	registeredTable := map[string]bool{}
	for _, c := range encryptedColumns {
		registeredTable[c.table] = true
	}
	for _, m := range reEncryptedComment.FindAllStringSubmatch(string(snapshot), -1) {
		if !registeredTable[m[1]] && notEncryptedTable[m[1]] == "" {
			t.Errorf("the schema says table %s holds encrypted values, and no column of it is on encryptedColumns", m[1])
		}
	}

	if len(missing) > 0 {
		t.Errorf("these columns are named like stored credentials and are on neither\n"+
			"encryptedColumns nor the exception list in this test:\n\t%s\n"+
			"If a column is a ciphertext it belongs in the registry, in the same change "+
			"that added it. If it is not, say so in notCiphertext and say why.",
			strings.Join(missing, "\n\t"))
	}
}

var (
	reRegistryTable  = regexp.MustCompile(`^CREATE TABLE public\.([a-z0-9_]+) \($`)
	reCredentialName = regexp.MustCompile(`_(secret|tokens?)$`)
	reEncryptedComment = regexp.MustCompile(`(?m)^COMMENT ON TABLE public\.([a-z0-9_]+) IS '[^\n]*[Ee]ncrypt`)
)

// credentialShapedColumns returns every "table.column" in the snapshot whose
// column name is spelled like a stored credential.
func credentialShapedColumns(text string) []string {
	var out []string
	table := ""
	for _, line := range strings.Split(text, "\n") {
		if m := reRegistryTable.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		if table == "" {
			continue
		}
		if strings.HasPrefix(line, ");") {
			table = ""
			continue
		}
		field := strings.TrimSpace(line)
		if field == "" || strings.HasPrefix(field, "CONSTRAINT ") {
			continue
		}
		name := strings.SplitN(field, " ", 2)[0]
		if name != "" && reCredentialName.MatchString(name) {
			out = append(out, table+"."+name)
		}
	}
	return out
}
