package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
)

// `toft rotate-key` — the sweep that finishes a rotation.
//
// Setting a new key is a restart (ADR §62): every record names the key that
// wrote it, so the old and the new are live at once and nothing breaks at the
// moment of the swap. What that leaves is an estate under two keys, and this is
// what empties the old half.
//
// **Rotation is not revocation, and the difference is this command's whole
// reason for reporting the way it does.** Until the sweep finishes, the retired
// key still opens records and must stay in ENCRYPTION_KEY_OLD; a key believed
// compromised is neutralised when this reports nothing left under it and the
// operator drops it, not when the new key was set. So the report names the two
// moments separately and never implies the first one was the second.
//
// It is a subcommand of the platform binary rather than of `toft`: it needs the
// database and the key material, and `toft` is a remote client holding an `toftu_`
// key. The CLI's rule that a credential is never a flag is untouched by this.
//
// It is not automatic on startup. A background re-encryption pass would make
// rotation one restart, and it would also put a long write load on a large
// install that nobody asked for at the moment they least want a surprise. An
// operator rotating a key is present by definition.

const rotateKeyUsage = `Usage: toft rotate-key [--dry-run]

Re-encrypt every stored secret under the current ENCRYPTION_KEY, so the keys in
ENCRYPTION_KEY_OLD can be dropped.

Rotating is two steps and this is the second. First: put the new key in
ENCRYPTION_KEY, move the previous one into ENCRYPTION_KEY_OLD, and restart —
from that moment every new write is under the new key and every old record
still reads. Then run this, with the same environment the server has, to move
what is left.

Flags:
  --dry-run   Report what is stored under which key and change nothing.

Safe to interrupt and safe to run twice: a record already under the current key
is skipped, so a re-run resumes rather than restarts. It refuses to begin if any
stored record was written under a key that is in neither variable — rewriting
what it can read while leaving the rest would turn a diagnosable problem into a
half-swept estate.

On a managed install (ENCRYPTION_KEY_URI set) this is also the move from a local
key to a KMS, with no special case: leave the old key in ENCRYPTION_KEY_OLD so it
keeps reading, and the sweep rewrites everything under the KMS-wrapped data key.

Reads DATABASE_URL, ENCRYPTION_KEY, ENCRYPTION_KEY_OLD and ENCRYPTION_KEY_URI
from the environment or .env, like the server does.
`

// runRotateKey sweeps the estate onto the current encrypt key.
func runRotateKey(args []string) error {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "report and change nothing")
	fs.Usage = func() { fmt.Fprint(os.Stderr, rotateKeyUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	db, err := database.Open(ctx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	defer db.Close()

	// The same keyring the server builds, from the same environment — including
	// the managed backend, where the encrypt key is a data key the KMS has to
	// unwrap. That is what makes the move from a local key to a KMS **this
	// command with no special case**: to the sweep the two backends are two key
	// ids, and it does not need to know which is which.
	enc, err := crypto.New(context.Background(), cfg.EncryptionKey, cfg.EncryptionKeyOld, crypto.Options{
		URI:  cfg.EncryptionKeyURI,
		DEKs: db,
	})
	if err != nil {
		return err
	}

	current := enc.EncryptKeyID()
	if enc.Backend() == crypto.BackendManaged {
		fmt.Printf("Backend:     managed by %s\n", enc.KEKURI())
	}
	fmt.Printf("Encrypt key: %s\nKeyring:     %v\n\n", current, enc.KeyIDs())

	// The survey comes first and always, dry run or not. A rotation that starts
	// writing before it knows what it cannot read is the failure mode this
	// whole design exists to remove.
	var (
		stored     []database.EncryptedSecret
		byKey      = map[string]int{}
		unreadable []database.EncryptedSecret
	)
	err = db.EachEncryptedSecret(func(s database.EncryptedSecret) error {
		stored = append(stored, s)
		byKey[crypto.RecordKeyID(s.Record)]++
		if !enc.CanRead(s.Record) {
			unreadable = append(unreadable, s)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if len(stored) == 0 {
		fmt.Println("Nothing is stored encrypted on this install. There is nothing to sweep.")
		return nil
	}

	fmt.Printf("%d stored secrets:\n", len(stored))
	for _, id := range sortedKeys(byKey) {
		label := id
		switch {
		case id == current:
			label += "  (the current key — nothing to do)"
		case id == crypto.LegacyKeyID:
			label += "  (written before key identifiers)"
		}
		fmt.Printf("  %6d under key %s\n", byKey[id], label)
	}
	fmt.Println()

	if len(unreadable) > 0 {
		// Refused rather than partially done. Whatever is wrong — a key dropped
		// from ENCRYPTION_KEY_OLD too early, a restore from a backup taken
		// under another key — the operator can still fix it while every record
		// is where it was, and cannot once half of them have moved.
		fmt.Printf("%d of them cannot be read with the keys this install has:\n", len(unreadable))
		shown := unreadable
		if len(shown) > 10 {
			shown = shown[:10]
		}
		for _, s := range shown {
			fmt.Printf("  %s (%s) — written under key %s\n",
				s.Where(), s.Class, crypto.RecordKeyID(s.Record))
		}
		if len(unreadable) > len(shown) {
			fmt.Printf("  … and %d more\n", len(unreadable)-len(shown))
		}
		return fmt.Errorf("refusing to sweep: put the missing keys in ENCRYPTION_KEY_OLD and run this again, " +
			"or re-enter those credentials if the key is gone for good")
	}

	pending := make([]database.EncryptedSecret, 0, len(stored))
	for _, s := range stored {
		if crypto.RecordKeyID(s.Record) != current {
			pending = append(pending, s)
		}
	}

	if len(pending) == 0 {
		fmt.Printf("Everything is already under key %s.\n\n", current)
		reportDone(cfg)
		return nil
	}

	if *dryRun {
		fmt.Printf("%d would be re-encrypted under key %s. Nothing was changed.\n", len(pending), current)
		return nil
	}

	fmt.Printf("Re-encrypting %d under key %s…\n", len(pending), current)
	var rewritten, skipped int
	for i, s := range pending {
		plain, err := enc.Decrypt(s.Record)
		if err != nil {
			// The survey said this was readable, so reaching here means the
			// record changed underneath us or the bytes are damaged. Either
			// way, stop: the estate is consistent at this point and will not be
			// after another write.
			return fmt.Errorf("%s (%s): %w", s.Where(), s.Class, err)
		}
		record, err := enc.Encrypt(plain)
		if err != nil {
			return fmt.Errorf("%s (%s): %w", s.Where(), s.Class, err)
		}
		changed, err := db.RewriteEncryptedSecret(s, record)
		if err != nil {
			return err
		}
		if changed {
			rewritten++
		} else {
			// Somebody saved that credential while this was running, so it is
			// already under the current key. Not an error, and worth counting.
			skipped++
		}
		if n := i + 1; n%100 == 0 || n == len(pending) {
			fmt.Printf("  %d/%d\n", n, len(pending))
		}
	}

	fmt.Printf("\nRe-encrypted %d", rewritten)
	if skipped > 0 {
		fmt.Printf(", %d were changed by someone else while this ran and are already current", skipped)
	}
	fmt.Println(".")
	reportDone(cfg)
	return nil
}

// reportDone says the one thing an operator is running this to find out, and
// says it only when it is true.
func reportDone(cfg *config.Config) {
	if len(cfg.EncryptionKeyOld) == 0 {
		fmt.Println("ENCRYPTION_KEY_OLD is empty, so there is nothing left to retire.")
		return
	}
	fmt.Printf("Nothing is stored under the %d retired key(s) any more. "+
		"Remove ENCRYPTION_KEY_OLD and restart to finish the rotation — "+
		"until you do, those keys still open nothing but are still configured.\n",
		len(cfg.EncryptionKeyOld))
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
